package web

import "embed"

// Files contains the browser UI bundled into the server binary.
//
//go:embed index.html css/style.css js/app.js js/vendor
var Files embed.FS
