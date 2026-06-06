//go:build cli_integration
// +build cli_integration

package tests

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
)

// acmeCorpConfig holds parsed credentials from PUFFERFS_INTEGRATION_TEST_S3_ENV.
type acmeCorpConfig struct {
	accessKeyID     string
	secretAccessKey string
	endpointURL     string
	bucketName      string
}

type objectEntry struct {
	key  string
	size int64
}

// parseIntegrationTestS3Env parses the PUFFERFS_INTEGRATION_TEST_S3_ENV env var,
// which is a space-separated set of KEY=VALUE pairs.
func parseIntegrationTestS3Env(t *testing.T) *acmeCorpConfig {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv("PUFFERFS_INTEGRATION_TEST_S3_ENV"))
	if raw == "" {
		return nil
	}

	env := make(map[string]string)
	// The env var may be newline-separated or space-separated KEY=VALUE pairs.
	// Try newline-separated first; fall back to splitting on " AWS_".
	var parts []string
	if strings.Contains(raw, "\n") {
		parts = strings.Split(raw, "\n")
	} else {
		// Space-separated: split on " AWS_" boundaries, keeping the key prefix.
		parts = splitOnKeyBoundary(raw)
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		idx := strings.Index(part, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(part[:idx])
		val := strings.TrimSpace(part[idx+1:])
		env[key] = val
	}

	cfg := &acmeCorpConfig{
		accessKeyID:     env["AWS_ACCESS_KEY_ID"],
		secretAccessKey: env["AWS_SECRET_ACCESS_KEY"],
		endpointURL:     env["AWS_ENDPOINT_URL"],
		bucketName:      env["AWS_BUCKET_NAME"],
	}
	if cfg.accessKeyID == "" || cfg.secretAccessKey == "" || cfg.endpointURL == "" || cfg.bucketName == "" {
		return nil
	}
	return cfg
}

func newAcmeCorpS3Client(t *testing.T, cfg *acmeCorpConfig) *s3sdk.Client {
	t.Helper()

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("auto"),
		awsconfig.WithCredentialsProvider(awscreds.NewStaticCredentialsProvider(cfg.accessKeyID, cfg.secretAccessKey, "")),
	)
	if err != nil {
		t.Fatalf("loading acme-corp S3 config: %v", err)
	}
	return s3sdk.NewFromConfig(awsCfg, func(o *s3sdk.Options) {
		o.BaseEndpoint = aws.String(cfg.endpointURL)
	})
}

// downloadAcmeCorp downloads all objects under "acme-corp/" from the integration
// test bucket into a local directory, returning the list of relative paths downloaded.
func downloadAcmeCorp(t *testing.T, cfg *acmeCorpConfig, destDir string) []string {
	t.Helper()

	ctx := context.Background()
	client := newAcmeCorpS3Client(t, cfg)

	prefix := "acme-corp/"
	paginator := s3sdk.NewListObjectsV2Paginator(client, &s3sdk.ListObjectsV2Input{
		Bucket: aws.String(cfg.bucketName),
		Prefix: aws.String(prefix),
	})

	listStart := time.Now()
	var objects []objectEntry
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			t.Fatalf("listing acme-corp objects in s3://%s/%s: %v", cfg.bucketName, prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil || strings.HasSuffix(*obj.Key, "/") {
				continue
			}
			var size int64
			if obj.Size != nil {
				size = *obj.Size
			}
			objects = append(objects, objectEntry{key: *obj.Key, size: size})
		}
	}
	t.Logf("timing stage=acme_corp_list bucket=%s prefix=%s objects=%d elapsed=%s",
		cfg.bucketName, prefix, len(objects), time.Since(listStart))

	if len(objects) == 0 {
		t.Fatalf("no objects found under s3://%s/%s", cfg.bucketName, prefix)
	}

	sort.Slice(objects, func(i, j int) bool { return objects[i].key < objects[j].key })
	objects = selectAcmeCorpObjects(objects)
	t.Logf("timing stage=acme_corp_select objects=%d full_corpus=%t",
		len(objects), strings.EqualFold(strings.TrimSpace(os.Getenv("PUFFERFS_E2E_ACME_CORP_FULL_CORPUS")), "1"))
	if len(objects) == 0 {
		t.Fatalf("no selected acme-corp objects under s3://%s/%s", cfg.bucketName, prefix)
	}

	var relPaths []string
	downloadStart := time.Now()
	var totalBytes int64
	for _, obj := range objects {
		relPath := strings.TrimPrefix(obj.key, prefix)
		fullPath := filepath.Join(destDir, filepath.FromSlash(relPath))
		dir := filepath.Dir(fullPath)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating dir %s: %v", dir, err)
		}

		resp, err := client.GetObject(ctx, &s3sdk.GetObjectInput{
			Bucket: aws.String(cfg.bucketName),
			Key:    aws.String(obj.key),
		})
		if err != nil {
			t.Fatalf("downloading s3://%s/%s: %v", cfg.bucketName, obj.key, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("reading s3://%s/%s: %v", cfg.bucketName, obj.key, err)
		}
		if err := os.WriteFile(fullPath, body, 0o644); err != nil {
			t.Fatalf("writing %s: %v", fullPath, err)
		}
		totalBytes += int64(len(body))
		relPaths = append(relPaths, relPath)
	}
	t.Logf("timing stage=acme_corp_download files=%d total_bytes=%d elapsed=%s",
		len(relPaths), totalBytes, time.Since(downloadStart))

	return relPaths
}

func selectAcmeCorpObjects(objects []objectEntry) []objectEntry {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("PUFFERFS_E2E_ACME_CORP_FULL_CORPUS")), "1") {
		return objects
	}

	var selected []objectEntry
	for _, object := range objects {
		if acmeCorpSmokeFixture(object.key) {
			selected = append(selected, object)
		}
	}
	return selected
}

func acmeCorpSmokeFixture(key string) bool {
	base := strings.ToLower(filepath.Base(key))
	ext := strings.ToLower(filepath.Ext(key))
	if base == ".gitignore" {
		return true
	}
	switch ext {
	case ".md", ".txt", ".csv", ".tsv", ".html", ".eml", ".ics", ".vcf":
		return true
	default:
		return false
	}
}

// TestAcmeCorpSync downloads the acme-corp test directory from the
// pufferfs-integration-test R2 bucket and syncs it through the full pipeline:
// local MinIO (storage) + Postgres (DB) + dev Modal (embedding/query embedding).
// By default it uses text-like fixtures from acme-corp so the deployed Modal app
// does not need direct write access to local MinIO for generated image artifacts.
// Set PUFFERFS_E2E_ACME_CORP_FULL_CORPUS=1 to include PDFs, Office documents,
// images, audio, and video when Modal's S3 secret is configured for writable storage.
//
// Required env vars:
//   - PUFFERFS_INTEGRATION_TEST_S3_ENV: R2 credentials for the integration test bucket
//   - MODAL_CHUNK_ENDPOINT, MODAL_EMBED_ENDPOINT, MODAL_QUERY_EMBED_ENDPOINT: Modal dev endpoints
//   - TURBOPUFFER_API_KEY: for indexing and querying
func TestAcmeCorpSync(t *testing.T) {
	acmeCfg := parseIntegrationTestS3Env(t)
	if acmeCfg == nil {
		t.Skip("PUFFERFS_INTEGRATION_TEST_S3_ENV not set or incomplete; skipping acme-corp integration test")
	}

	services := requireRealServices(t)
	setupE2EInfra(t)

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	env := newE2EEnv(t, services, "")
	homeDir := t.TempDir()
	initPufferFS(t, env, homeDir)

	// Download acme-corp from R2 to a local directory.
	projectDir := filepath.Join(homeDir, "acme-corp-"+suffix)
	relPaths := downloadAcmeCorp(t, acmeCfg, projectDir)
	t.Logf("downloaded %d files from acme-corp to %s", len(relPaths), projectDir)

	// Sync the downloaded directory.
	syncStart := time.Now()
	stdout, stderr, err := runPufferfs(t, homeDir, env.serverURL, env.apiKey, "sync", projectDir, "--name", env.rootName)
	t.Logf("timing stage=acme_corp_sync elapsed=%s", time.Since(syncStart))
	if err != nil {
		t.Fatalf("acme-corp sync failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	requireOutputContains(t, stdout, "Sync complete")

	rootID := resolveRootID(t, env.serverURL, env.apiKey, env.rootName)
	namespaces := rootIndexNamespaces(t, rootID)
	t.Logf("root %s has %d namespace shards", rootID, len(namespaces))
	cleanupDone := false
	t.Cleanup(func() {
		if !cleanupDone {
			adminDelete(t, env.serverURL, "/admin/orgs/"+url.PathEscape(env.orgID))
			deleteTPNamespaces(t, services, namespaces)
		}
	})

	// Verify storage artifacts exist.
	assertMinioHasPrefix(t, fmt.Sprintf("bundles/%s/", rootID))
	assertMinioHasPrefix(t, "syncs/")

	// Verify indexing completed for a sample of known files.
	samplePaths := []string{
		"README.md",
		"documentation/engineering/deployment_runbook.md",
		"documentation/process/onboarding_process.md",
		"documentation/process/sales_process.md",
		"data/raw/customers.csv",
		"data/raw/data_dictionary.txt",
		"web/reports/monthly_report.html",
		"communications/email/welcome_email.eml",
		"communications/calendar/all_hands_meeting.ics",
		"communications/contacts/john_smith.vcf",
		"shared/style_guide.md",
		"archives/2024_q1/q1_summary.md",
	}
	for _, p := range samplePaths {
		assertHasTPRows(t, services, namespaces, p)
	}

	assertAcmeCorpQueries(t, homeDir, env)

	// Cleanup.
	deleteCreatedDataAndAssertGone(t, env.serverURL, env.orgID, []string{env.userID}, []string{rootID})
	cleanupDone = true
}

func assertAcmeCorpQueries(t *testing.T, homeDir string, env *e2eEnv) {
	t.Helper()

	cases := []struct {
		name         string
		query        string
		mode         string
		glob         string
		expectedPath string
	}{
		{
			name:         "readme role navigation",
			query:        "Sales Team Sales Dashboard Customer Data Engineering Team Deployment Guide",
			mode:         "fts",
			expectedPath: "README.md",
		},
		{
			name:         "engineering runbook",
			query:        "production deployment rollback procedures database backup kubectl smoke tests",
			mode:         "hybrid",
			expectedPath: "documentation/engineering/deployment_runbook.md",
		},
		{
			name:         "onboarding process",
			query:        "30 60 90 day goals buddy assigned pre boarding IT provisions equipment accounts",
			mode:         "fts",
			glob:         "documentation/process/**",
			expectedPath: "documentation/process/onboarding_process.md",
		},
		{
			name:         "sales process",
			query:        "BANT criteria discovery call demo proposal negotiation closed won Salesforce",
			mode:         "fts",
			glob:         "documentation/process/**",
			expectedPath: "documentation/process/sales_process.md",
		},
		{
			name:         "customer csv",
			query:        "Acme Manufacturing John Smith Manufacturing annual revenue active customer",
			mode:         "fts",
			glob:         "data/**",
			expectedPath: "data/raw/customers.csv",
		},
		{
			name:         "data dictionary",
			query:        "Salesforce CRM export account status product catalog TSV transactions salesperson region",
			mode:         "fts",
			glob:         "data/**",
			expectedPath: "data/raw/data_dictionary.txt",
		},
		{
			name:         "monthly html report",
			query:        "monthly revenue churn rate NPS score enterprise deals customer acquisition cost",
			mode:         "fts",
			glob:         "web/**",
			expectedPath: "web/reports/monthly_report.html",
		},
		{
			name:         "email fixture",
			query:        "welcome aboard laptop equipment pickup photo ID signed offer letter",
			mode:         "fts",
			glob:         "communications/**",
			expectedPath: "communications/email/welcome_email.eml",
		},
		{
			name:         "calendar fixture",
			query:        "Q3 All-Hands Meeting company-wide quarterly update 2025 goals Q&A",
			mode:         "fts",
			glob:         "communications/**",
			expectedPath: "communications/calendar/all_hands_meeting.ics",
		},
		{
			name:         "contact fixture",
			query:        "John Smith Senior Account Executive enterprise accounts sales",
			mode:         "fts",
			glob:         "communications/**",
			expectedPath: "communications/contacts/john_smith.vcf",
		},
		{
			name:         "style guide",
			query:        "Primary Navy Secondary Teal Accent Orange writing voice concise friendly",
			mode:         "fts",
			glob:         "shared/**",
			expectedPath: "shared/style_guide.md",
		},
		{
			name:         "archive summary",
			query:        "January March 2024 net income opened East region sales office historical record",
			mode:         "fts",
			glob:         "archives/**",
			expectedPath: "archives/2024_q1/q1_summary.md",
		},
		{
			name:         "semantic hybrid",
			query:        "how should I deploy production safely and roll back if the release fails",
			mode:         "hybrid",
			glob:         "documentation/engineering/**",
			expectedPath: "documentation/engineering/deployment_runbook.md",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertCLIQuery(t, homeDir, env, tc.query, env.rootName, tc.mode, tc.glob, tc.expectedPath)
		})
	}
}

// splitOnKeyBoundary splits a single-line "KEY=val KEY2=val2" string on spaces
// that precede an uppercase KEY= token. Go's regexp doesn't support lookaheads,
// so we do it manually.
func splitOnKeyBoundary(s string) []string {
	var parts []string
	start := 0
	for i := 1; i < len(s); i++ {
		if s[i-1] == ' ' && i < len(s) && isUpperOrUnderscore(s[i]) {
			// Check if this starts a KEY= token
			eqIdx := strings.Index(s[i:], "=")
			if eqIdx > 0 && eqIdx < 40 {
				allUpper := true
				for _, c := range s[i : i+eqIdx] {
					if !isUpperOrUnderscore(byte(c)) {
						allUpper = false
						break
					}
				}
				if allUpper {
					parts = append(parts, s[start:i-1])
					start = i
				}
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func isUpperOrUnderscore(b byte) bool {
	return (b >= 'A' && b <= 'Z') || b == '_'
}
