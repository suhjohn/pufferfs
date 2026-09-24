package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pufferfs/pufferfs/internal/ignore"
	"github.com/pufferfs/pufferfs/pkg/models"
	bolt "go.etcd.io/bbolt"
)

var catalogFilesBucket = []byte("files")
var catalogMetaBucket = []byte("meta")
var catalogPendingBucket = []byte("pending")

// Metadata only. Source bytes and accepted capture journals keep their existing
// independent retention. One transaction saves a delta page AND its cursor, so
// process interruption cannot advance the cursor past missing local records.
type capturedCatalog struct {
	db     *bolt.DB
	client *apiClient
	rootID string
}

func (c *capturedCatalog) policyStamp(policy ignore.PolicyPatternSet) ([]byte, bool, error) {
	encoded, err := json.Marshal(policy)
	if err != nil {
		return nil, false, err
	}
	if home, err := os.UserHomeDir(); err == nil {
		global, err := os.ReadFile(filepath.Join(home, ".tpfs", ".tpfsignore"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, false, err
		}
		encoded = append(append(encoded, 0), global...)
	}
	stamp := sha256.Sum256(encoded)
	changed := false
	err = c.db.View(func(tx *bolt.Tx) error {
		changed = !bytes.Equal(tx.Bucket(catalogMetaBucket).Get([]byte("policy")), stamp[:])
		return nil
	})
	return stamp[:], changed, err
}

func (c *capturedCatalog) savePolicy(stamp []byte) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(catalogMetaBucket).Put([]byte("policy"), stamp); err != nil {
			return err
		}
		if err := tx.DeleteBucket(catalogPendingBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucket(catalogPendingBucket)
		return err
	})
}

func openCapturedCatalog(input captureSyncInput, cacheDir string) (*capturedCatalog, error) {
	db, err := bolt.Open(filepath.Join(cacheDir, "remote-catalog.db"), 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	catalog := &capturedCatalog{db: db, client: input.Client, rootID: input.RootID}
	err = db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(catalogMetaBucket)
		if err != nil {
			return err
		}
		identity := []byte("1\x00" + input.Client.baseURL + "\x00" + input.RootID)
		if existing := meta.Get([]byte("identity")); existing != nil && !bytes.Equal(existing, identity) {
			return errors.New("remote catalog cache identity mismatch")
		}
		if err = meta.Put([]byte("identity"), identity); err != nil {
			return err
		}
		if _, err = tx.CreateBucketIfNotExists(catalogPendingBucket); err != nil {
			return err
		}
		_, err = tx.CreateBucketIfNotExists(catalogFilesBucket)
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return catalog, nil
}

func (c *capturedCatalog) refresh(ctx context.Context) (map[string]bool, bool, error) {
	changed := make(map[string]bool)
	reset := false
	var cursor string
	if err := c.db.View(func(tx *bolt.Tx) error {
		cursor = string(tx.Bucket(catalogMetaBucket).Get([]byte("cursor")))
		// Fetching metadata is not a completed sync. Keep these paths until
		// capture succeeds, including across network errors and process exits.
		return tx.Bucket(catalogPendingBucket).ForEach(func(key, _ []byte) error {
			changed[string(key)] = true
			return nil
		})
	}); err != nil {
		return nil, false, err
	}
	for {
		query := url.Values{"limit": {"500"}, "cursor": {cursor}}
		body, err := c.client.requestWithContext(ctx, http.MethodGet, "/roots/"+url.PathEscape(c.rootID)+"/catalog-changes?"+query.Encode(), nil)
		if err != nil {
			var response *apiError
			var problem struct {
				Code string `json:"code"`
			}
			if reset || !errors.As(err, &response) || response.StatusCode != http.StatusConflict || json.Unmarshal(response.Body, &problem) != nil || problem.Code != "catalog_cursor_reset" {
				return nil, reset, err
			}
			if err = c.db.Update(func(tx *bolt.Tx) error {
				if err := tx.DeleteBucket(catalogFilesBucket); err != nil {
					return err
				}
				if _, err := tx.CreateBucket(catalogFilesBucket); err != nil {
					return err
				}
				if err := tx.DeleteBucket(catalogPendingBucket); err != nil {
					return err
				}
				if _, err := tx.CreateBucket(catalogPendingBucket); err != nil {
					return err
				}
				return tx.Bucket(catalogMetaBucket).Delete([]byte("cursor"))
			}); err != nil {
				return nil, reset, err
			}
			cursor = ""
			clear(changed)
			reset = true
			continue
		}
		var page models.CatalogChangesResponse
		if err = json.Unmarshal(body, &page); err != nil {
			return nil, reset, err
		}
		if page.Cursor == "" || len(page.Cursor) > 2048 || len(page.Files) > 500 {
			return nil, reset, errors.New("invalid catalog changes response")
		}
		if len(page.Files) > 0 || page.Cursor != cursor {
			err = c.db.Update(func(tx *bolt.Tx) error {
				files := tx.Bucket(catalogFilesBucket)
				for _, file := range page.Files {
					if !validCachedCatalogFile(file) {
						return errors.New("invalid changed catalog file")
					}
					encoded, err := json.Marshal(file)
					if err != nil {
						return err
					}
					if err = files.Put([]byte(file.Path), encoded); err != nil {
						return err
					}
					if err = tx.Bucket(catalogPendingBucket).Put([]byte(file.Path), []byte{1}); err != nil {
						return err
					}
					changed[file.Path] = true
				}
				return tx.Bucket(catalogMetaBucket).Put([]byte("cursor"), []byte(page.Cursor))
			})
			if err != nil {
				return nil, reset, err
			}
		}
		if !page.More {
			return changed, reset, nil
		}
		if page.Cursor == cursor {
			return nil, reset, errors.New("catalog changes did not advance")
		}
		cursor = page.Cursor
	}
}

func validCachedCatalogFile(file models.CapturedFileHead) bool {
	return file.Path != "." && filepath.IsLocal(file.Path) && filepath.ToSlash(filepath.Clean(file.Path)) == file.Path &&
		file.FileID != "" && file.VersionID != "" && file.Sequence > 0 && file.Size >= 0 &&
		(file.Deleted || validContentHash(file.ContentHash))
}

// Nil paths means a full local metadata scan. A changed directory uses an
// ordered prefix lookup, including removed descendants, without scanning the
// unrelated catalog. The callback must not mutate this cache.
func (c *capturedCatalog) walk(paths []string, visit func(models.CapturedFileHead) error) error {
	return c.db.View(func(tx *bolt.Tx) error {
		files := tx.Bucket(catalogFilesBucket)
		decode := func(key, value []byte) error {
			var file models.CapturedFileHead
			if err := json.Unmarshal(value, &file); err != nil {
				return err
			}
			if !validCachedCatalogFile(file) || file.Path != string(key) {
				return fmt.Errorf("invalid cached catalog record")
			}
			return visit(file)
		}
		if paths == nil {
			return files.ForEach(decode)
		}
		for _, path := range paths {
			if value := files.Get([]byte(path)); value != nil {
				if err := decode([]byte(path), value); err != nil {
					return err
				}
			}
			prefix := path + "/"
			cursor := files.Cursor()
			for key, value := cursor.Seek([]byte(prefix)); key != nil && strings.HasPrefix(string(key), prefix); key, value = cursor.Next() {
				if err := decode(key, value); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
