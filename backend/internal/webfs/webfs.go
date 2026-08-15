// Package webfs serves the embedded frontend build. In M0 the embedded dist
// tree contains only a placeholder index.html; the real Vue build is placed
// here by the build pipeline in later milestones.
package webfs

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"

	"github.com/gin-gonic/gin"
)

//go:embed dist
var distFS embed.FS

// content is distFS re-rooted at dist/.
var content fs.FS

func init() {
	var err error
	content, err = fs.Sub(distFS, "dist")
	if err != nil {
		// Unreachable: dist embeds at least index.html.
		panic(err)
	}
}

// Register mounts the embedded frontend on r: / serves index.html and
// /assets/* serves static assets.
//
// Files are served directly via fs.ReadFile instead of http.FileServer: the
// latter redirects any request path ending in "/index.html" to "./", which
// loops forever when serving the SPA entry point at "/".
func Register(r *gin.Engine) {
	r.GET("/", index)
	r.GET("/assets/*filepath", asset)
}

func index(c *gin.Context) {
	b, err := fs.ReadFile(content, "index.html")
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", b)
}

func asset(c *gin.Context) {
	// c.Param("filepath") includes the leading "/".
	name := "assets" + c.Param("filepath")
	b, err := fs.ReadFile(content, name)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	c.Data(http.StatusOK, ct, b)
}
