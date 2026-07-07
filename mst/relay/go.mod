module github.com/hansrajrami/fabric/mst/relay

go 1.25.0

require (
	github.com/hansrajrami/fabric/mst/canonical v0.0.0
	github.com/hansrajrami/fabric/mst/fabric-chaincode/proofhelper v0.0.0-00010101000000-000000000000
	github.com/hyperledger/fabric-gateway v1.11.0
	github.com/hyperledger/fabric-protos-go-apiv2 v0.3.7
	github.com/syndtr/goleveldb v1.0.1-0.20210819022825-2ae1ddf74ef7
	google.golang.org/grpc v1.82.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/golang/snappy v0.0.4 // indirect
	github.com/miekg/pkcs11 v1.1.2 // indirect
	golang.org/x/crypto v0.50.0 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
)

replace github.com/hansrajrami/fabric/mst/canonical => ../canonical

replace github.com/hansrajrami/fabric/mst/fabric-chaincode/proofhelper => ../fabric-chaincode/proofhelper
