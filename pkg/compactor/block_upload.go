// SPDX-License-Identifier: AGPL-3.0-only

package compactor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/go-kit/log/level"
	"github.com/gofrs/uuid"
	"github.com/gorilla/mux"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/thanos-io/thanos/pkg/block/metadata"

	"github.com/grafana/dskit/tenant"
	"github.com/grafana/regexp"

	"github.com/grafana/mimir/pkg/storage/bucket"
	mimir_tsdb "github.com/grafana/mimir/pkg/storage/tsdb"
)

// CreateBlockUpload handles requests for creating block upload sessions.
func (c *MultitenantCompactor) CreateBlockUpload(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	blockID := vars["block"]
	if blockID == "" {
		http.Error(w, "missing block ID", http.StatusBadRequest)
		return
	}
	tenantID, ctx, err := tenant.ExtractTenantIDFromHTTPRequest(r)
	if err != nil {
		http.Error(w, "invalid tenant ID", http.StatusBadRequest)
		return
	}

	level.Debug(c.logger).Log("msg", "creating block upload session", "user", tenantID, "block_id", blockID)

	bkt := bucket.NewUserBucketClient(string(tenantID), c.bucketClient, c.cfgProvider)
	exists := false
	if err := bkt.Iter(ctx, blockID, func(pth string) error {
		exists = true
		return nil
	}); err != nil {
		level.Error(c.logger).Log("msg", "failed to iterate over block files", "user", tenantID,
			"block_id", blockID, "err", err)
		http.Error(w, "failed iterating over block files in object storage", http.StatusBadGateway)
		return
	}
	if exists {
		level.Debug(c.logger).Log("msg", "block already exists in object storage", "user", tenantID,
			"block_id", blockID)
		http.Error(w, "block already exists in object storage", http.StatusConflict)
		return
	}

	rnd, err := uuid.NewV4()
	if err != nil {
		level.Error(c.logger).Log("msg", "failed to generate UUID", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	lockName := fmt.Sprintf("%s.lock", rnd)
	if err := bkt.Upload(ctx, lockName, bytes.NewBuffer(nil)); err != nil {
		level.Error(c.logger).Log("msg", "failed to upload lock file to block dir", "user", tenantID,
			"block_id", blockID, "err", err)
		http.Error(w, "failed uploading lock file to block dir", http.StatusBadGateway)
		return
	}
	if err := bkt.Iter(ctx, blockID, func(pth string) error {
		exists = pth != lockName
		return nil
	}); err != nil {
		level.Error(c.logger).Log("msg", "failed to iterate over block files", "user", tenantID,
			"block_id", blockID, "err", err)
		http.Error(w, "failed iterating over block files in object storage", http.StatusBadGateway)
		return
	}
	if exists {
		level.Debug(c.logger).Log("msg", "another file exists for block in object storage", "user", tenantID,
			"block_id", blockID)
		http.Error(w, "another file exists for block in object storage", http.StatusConflict)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// UploadBlockFile handles requests for uploading block files.
func (c *MultitenantCompactor) UploadBlockFile(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	blockID := vars["block"]
	if blockID == "" {
		http.Error(w, "missing block ID", http.StatusBadRequest)
		return
	}
	pth, err := url.QueryUnescape(vars["path"])
	if err != nil {
		http.Error(w, fmt.Sprintf("malformed file path: %q", vars["path"]), http.StatusBadRequest)
		return
	}
	if pth == "" {
		http.Error(w, "missing file path", http.StatusBadRequest)
		return
	}

	tenantID, ctx, err := tenant.ExtractTenantIDFromHTTPRequest(r)
	if err != nil {
		http.Error(w, "invalid tenant ID", http.StatusBadRequest)
		return
	}

	if path.Base(pth) == "meta.json" {
		http.Error(w, "meta.json is not allowed", http.StatusBadRequest)
		return
	}

	rePath := regexp.MustCompile(`^(index|chunks/\d{6})$`)
	if !rePath.MatchString(pth) {
		http.Error(w, fmt.Sprintf("invalid path: %q", pth), http.StatusBadRequest)
		return
	}

	if r.Body == nil || r.ContentLength == 0 {
		http.Error(w, "file cannot be empty", http.StatusBadRequest)
		return
	}

	bkt := bucket.NewUserBucketClient(string(tenantID), c.bucketClient, c.cfgProvider)

	exists := false
	if err := bkt.Iter(ctx, blockID, func(pth string) error {
		exists = strings.HasSuffix(pth, ".lock")
		return nil
	}); err != nil {
		level.Error(c.logger).Log("msg", "failed to iterate over block files", "user", tenantID,
			"block_id", blockID, "err", err)
		http.Error(w, "failed iterating over block files in object storage", http.StatusBadGateway)
		return
	}
	if !exists {
		level.Debug(c.logger).Log("msg", "no lock file exists for block in object storage, refusing file upload",
			"user", tenantID, "block_id", blockID)
		http.Error(w, "block upload has not yet been initiated", http.StatusBadRequest)
		return
	}

	dst := path.Join(blockID, pth)

	level.Debug(c.logger).Log("msg", "uploading block file to bucket", "user", tenantID,
		"destination", dst, "size", r.ContentLength)
	reader := bodyReader{
		r: r,
	}
	if err := bkt.Upload(ctx, dst, reader); err != nil {
		level.Error(c.logger).Log("msg", "failed uploading block file to bucket",
			"user", tenantID, "destination", dst, "err", err)
		http.Error(w, "failed uploading block file to bucket", http.StatusBadGateway)
		return
	}

	level.Debug(c.logger).Log("msg", "finished uploading block file to bucket",
		"user", tenantID, "block_id", blockID, "path", pth)

	w.WriteHeader(http.StatusOK)
}

// CompleteBlockUpload handles a request to complete a block upload session.
func (c *MultitenantCompactor) CompleteBlockUpload(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	blockID := vars["block"]
	if blockID == "" {
		http.Error(w, "missing block ID", http.StatusBadRequest)
		return
	}

	tenantID, ctx, err := tenant.ExtractTenantIDFromHTTPRequest(r)
	if err != nil {
		http.Error(w, "invalid tenant ID", http.StatusBadRequest)
		return
	}

	level.Debug(c.logger).Log("msg", "received request to complete block upload", "user", tenantID,
		"block_id", blockID, "content_length", r.ContentLength)

	dec := json.NewDecoder(r.Body)
	var meta metadata.Meta
	if err := dec.Decode(&meta); err != nil {
		http.Error(w, "malformed request body", http.StatusBadRequest)
		return
	}

	bkt := bucket.NewUserBucketClient(tenantID, c.bucketClient, c.cfgProvider)

	exists := false
	if err := bkt.Iter(ctx, blockID, func(pth string) error {
		exists = strings.HasSuffix(pth, ".lock")
		return nil
	}); err != nil {
		level.Error(c.logger).Log("msg", "failed to iterate over block files", "user", tenantID,
			"block_id", blockID, "err", err)
		http.Error(w, "failed iterating over block files in object storage", http.StatusBadGateway)
		return
	}
	if !exists {
		level.Debug(c.logger).Log("msg", "no lock file exists for block in object storage, refusing to complete block",
			"user", tenantID, "block_id", blockID)
		http.Error(w, "block upload has not yet been initiated", http.StatusBadRequest)
		return
	}

	level.Debug(c.logger).Log("msg", "completing block upload", "user",
		tenantID, "block_id", blockID, "files", len(meta.Thanos.Files))

	if err := c.sanitizeMeta(&meta, blockID, tenantID); err != nil {
		level.Error(c.logger).Log("msg", "failed to sanitize meta.json", "user", tenantID,
			"block_id", blockID, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Write meta.json, so the block is considered complete
	dst := path.Join(blockID, "meta.json")
	level.Debug(c.logger).Log("msg", "writing meta.json in bucket", "dst", dst)
	buf := bytes.NewBuffer(nil)
	enc := json.NewEncoder(buf)
	if err := enc.Encode(meta); err != nil {
		level.Error(c.logger).Log("msg", "failed to encode meta.json", "user", tenantID,
			"block_id", blockID, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := bkt.Upload(ctx, dst, buf); err != nil {
		level.Error(c.logger).Log("msg", "failed uploading meta.json to bucket", "user", tenantID,
			"dst", dst, "err", err)
		http.Error(w, "failed uploading meta.json to bucket", http.StatusBadGateway)
		return
	}

	var lockFiles []string
	if err := bkt.Iter(ctx, blockID, func(pth string) error {
		if strings.HasSuffix(pth, ".lock") {
			lockFiles = append(lockFiles, pth)
		}
		return nil
	}); err != nil {
		level.Error(c.logger).Log("msg", "failed to iterate over block files", "user", tenantID,
			"block_id", blockID, "err", err)
		http.Error(w, "failed iterating over block files in object storage", http.StatusBadGateway)
		return
	}
	failed := false
	for _, lf := range lockFiles {
		if err := bkt.Delete(ctx, lf); err != nil {
			level.Error(c.logger).Log("msg", "failed to delete lock file from block in object storage", "user", tenantID,
				"block_id", blockID, "err", err)
			failed = true
		}
	}
	if failed {
		http.Error(w, "failed deleting lock file(s) from object storage", http.StatusBadGateway)
		return
	}

	level.Debug(c.logger).Log("msg", "successfully completed block upload")

	w.WriteHeader(http.StatusOK)
}

func (c *MultitenantCompactor) sanitizeMeta(meta *metadata.Meta, blockID, tenantID string) error {
	if meta.Thanos.Labels == nil {
		meta.Thanos.Labels = map[string]string{}
	}
	updated := false

	metaULID := meta.ULID
	if metaULID.String() != blockID {
		level.Warn(c.logger).Log("msg", "updating meta.json block ID", "old_value", metaULID.String(),
			"new_value", blockID)
		var err error
		meta.ULID, err = ulid.Parse(blockID)
		if err != nil {
			return errors.Wrapf(err, "couldn't parse block ID %q", blockID)
		}
		updated = true
	}

	metaTenantID := meta.Thanos.Labels[mimir_tsdb.TenantIDExternalLabel]
	if metaTenantID != tenantID {
		level.Warn(c.logger).Log("msg", "updating meta.json tenant label", "block_id", blockID,
			"old_value", metaTenantID, "new_value", tenantID)
		updated = true
		meta.Thanos.Labels[mimir_tsdb.TenantIDExternalLabel] = tenantID
	}

	for l, v := range meta.Thanos.Labels {
		switch l {
		case mimir_tsdb.TenantIDExternalLabel, mimir_tsdb.IngesterIDExternalLabel:
		case mimir_tsdb.CompactorShardIDExternalLabel:
			// TODO: Verify that all series are compatible with the shard ID
		default:
			level.Warn(c.logger).Log("msg", "removing unknown meta.json label", "block_id", blockID, "label", l, "value", v)
			updated = true
			delete(meta.Thanos.Labels, l)
		}
	}

	// TODO: List files in bucket and update file list in meta.json

	if !updated {
		level.Info(c.logger).Log("msg", "no changes to meta.json required", "block_id", blockID)
	}

	return nil
}

type bodyReader struct {
	r *http.Request
}

// ObjectSize implements thanos.ObjectSizer.
func (r bodyReader) ObjectSize() (int64, error) {
	return r.r.ContentLength, nil
}

// Read implements io.Reader.
func (r bodyReader) Read(b []byte) (int, error) {
	return r.r.Body.Read(b)
}