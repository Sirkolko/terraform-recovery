// Package web holds the embedded user interface: HTML templates, a small
// amount of vanilla JavaScript and CSS. Everything is compiled into the
// binary; nothing is loaded from the network.
package web

import "embed"

// Files contains templates/ and static/.
//
//go:embed templates/*.html static/*
var Files embed.FS
