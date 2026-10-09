// Package lint is the module that pins staticcheck for CI and make lint,
// together with the golang.org/x/tools it reads compiled packages with: a
// staticcheck release can lag a Go patch release's export format, as 0.8.1
// did Go 1.27.2. Build it with
//
//	go build -C scripts/lint -o staticcheck honnef.co/go/tools/cmd/staticcheck
//
// There is no code here; this file gives go vet a package to look at.
package lint
