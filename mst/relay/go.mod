module github.com/hansrajrami/fabric/mst/relay

go 1.24

require github.com/syndtr/goleveldb v1.0.1-0.20210819022825-2ae1ddf74ef7

require (
	github.com/golang/snappy v0.0.4 // indirect
	golang.org/x/sys v0.34.0 // indirect
	golang.org/x/text v0.27.0 // indirect
)

replace github.com/hansrajrami/fabric/mst/canonical => ../canonical
