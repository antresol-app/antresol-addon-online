module github.com/antresol-app/antresol-addon-online

go 1.27.0

toolchain go1.27.1

require (
	// the addonkit release the public build uses (go.sum keeps its two h1
	// lines once it is tagged); in the core repository the replace makes them
	// unused, so go mod tidy there would drop them
	github.com/antresol-app/antresol/addonkit v0.2.0
	golang.org/x/net v0.60.0
	golang.org/x/sync v0.24.0
	golang.org/x/text v0.43.0
	modernc.org/sqlite v1.61.0
)

require (
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.49.0 // indirect
	modernc.org/libc v1.79.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
