//go:build !ui

package webui

import "io/fs"

// embedded returns nil in builds without the "ui" tag: the handler serves a page that
// explains how to build the console.
func embedded() fs.FS { return nil }
