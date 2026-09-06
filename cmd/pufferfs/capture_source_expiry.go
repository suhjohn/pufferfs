package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

func retiredPackKeys(err error) []string {
	var apiErr *apiError
	var response struct {
		Code string   `json:"code"`
		Keys []string `json:"object_keys"`
	}
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict || json.Unmarshal(apiErr.Body, &response) != nil || response.Code != "source_pack_reupload_required" || len(response.Keys) == 0 {
		return nil
	}
	return response.Keys
}

// Only a definitive server retirement can reset upload identities. The source
// bytes, digests, capture ID, prior file versions and local extent map stay put.
func resetRetiredCapturePacks(dir string, journal *captureJournal, cause error) (bool, error) {
	keys := retiredPackKeys(cause)
	if len(keys) == 0 || journal.Accepted != nil {
		return false, nil
	}
	indices := make(map[int]bool)
	for _, key := range keys {
		found := false
		for index, pack := range journal.Packs {
			matches := pack.ObjectKey != "" && pack.ObjectKey == key
			if pack.ObjectKey == "" && pack.Multipart != nil {
				parts := strings.Split(key, "/")
				matches = len(parts) == 5 && parts[0] == "sources" && parts[2] == journal.RootID && parts[3] == "multipart" && parts[4] == pack.Multipart.RequestID
			}
			if matches {
				indices[index] = true
				found = true
				break
			}
		}
		// A remote append extent has no local replacement bytes. Do not
		// discard any identities or silently reread a changed filesystem path.
		if !found {
			return false, errors.New("retired source has no retained local pack; capture preserved for recovery")
		}
	}
	for index := range indices {
		pack := &journal.Packs[index]
		pack.ObjectKey, pack.Complete, pack.Multipart = "", false, nil
	}
	return true, saveCaptureJournal(dir, *journal)
}
