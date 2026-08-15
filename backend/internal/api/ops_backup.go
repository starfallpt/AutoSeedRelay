package api

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/autoseedrelay/relay/internal/backup"
	"github.com/autoseedrelay/relay/internal/store"
	"github.com/gin-gonic/gin"
)

// exportBackup handles GET /backup/export: it streams a zip snapshot of the
// database as an attachment. Export writes straight to the response writer, so
// the status is committed to 200 before any mid-stream failure; such a failure
// is logged and the client sees a truncated (unusable) archive.
func (o *Ops) exportBackup(c *gin.Context) {
	if !o.requireRepo(c) {
		return
	}
	c.Header("Content-Disposition", `attachment; filename="autoseedrelay-backup.zip"`)
	c.Header("Content-Type", "application/zip")
	if err := backup.Export(c.Request.Context(), o.Repo.DB(), c.Writer); err != nil {
		c.Error(err)
	}
}

// restoreBackup handles POST /backup/restore (multipart file): it validates the
// archive, then persists it to <dataDir>/restore-pending.zip. The actual restore
// runs on the next boot via a startup hook (wired by main, not here), which is
// why the response is {ok:true, restart_required:true}.
func (o *Ops) restoreBackup(c *gin.Context) {
	if !o.requireRepo(c) {
		return
	}
	fh, err := c.FormFile("file")
	if err != nil {
		opsWriteError(c, http.StatusBadRequest, "missing upload file")
		return
	}
	f, err := fh.Open()
	if err != nil {
		opsWriteError(c, http.StatusBadRequest, "open upload: "+err.Error())
		return
	}
	defer f.Close()

	// Buffer the upload (capped) so it can be validated once and then written
	// verbatim without re-reading the multipart part.
	data, err := io.ReadAll(io.LimitReader(f, backup.MaxBackupBytes+1))
	if err != nil {
		opsWriteError(c, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}
	if int64(len(data)) > backup.MaxBackupBytes {
		opsWriteError(c, http.StatusBadRequest, "upload too large")
		return
	}
	if err := backup.ValidateZip(bytes.NewReader(data)); err != nil {
		opsWriteError(c, http.StatusBadRequest, err.Error())
		return
	}

	dataDir := o.DataDir
	if dataDir == "" {
		dataDir = filepath.Dir(store.DefaultDBPath)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		opsWriteError(c, http.StatusInternalServerError, "create data dir: "+err.Error())
		return
	}

	// Write atomically: a temp file in the same directory, then rename over the
	// final name so a crash never leaves a half-written pending archive.
	dst := filepath.Join(dataDir, "restore-pending.zip")
	tmp, err := os.CreateTemp(dataDir, ".restore-pending-*.zip")
	if err != nil {
		opsWriteError(c, http.StatusInternalServerError, "create temp file: "+err.Error())
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		opsWriteError(c, http.StatusInternalServerError, "write restore file: "+err.Error())
		return
	}
	if err := tmp.Close(); err != nil {
		opsWriteError(c, http.StatusInternalServerError, "close restore file: "+err.Error())
		return
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		opsWriteError(c, http.StatusInternalServerError, "finalize restore file: "+err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true, "restart_required": true})
}
