package main

import (
	"log"

	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
)

func main() {
	chaincode, err := contractapi.NewChaincode(&SmartContract{})
	if err != nil {
		log.Fatalf("create mst-example chaincode: %v", err)
	}
	if err := chaincode.Start(); err != nil {
		log.Fatalf("start mst-example chaincode: %v", err)
	}
}
