package app

import "embed"

// templatesFS embeds the SSR templates so the panel binary runs from any
// working directory (previously the CWD-relative path forced running from the
// repo root and the Dockerfile had to copy the template tree).
//
//go:embed templates/*.tmpl templates/partials/*.tmpl
var templatesFS embed.FS
