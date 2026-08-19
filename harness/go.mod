module github.com/webloomlabs/uptime-cairn/harness

go 1.25.0

// Dependencies are deliberately not pinned by hand here — run `go mod tidy` once
// and commit the resulting go.mod/go.sum. See README.md for the single
// third-party package this tool uses and the justification for it.

require modernc.org/sqlite v1.56.0

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
