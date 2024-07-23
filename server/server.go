package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/openfluxcd/controller-manager/storage"
)

type StartServer func(path, address string) error

// InitializeStorage creates a storage and returns the means to launch a file server to serve created Artifacts.
func InitializeStorage(path, storageAddress string, artifactRetentionTTL time.Duration, artifactRetentionRecords int) (StartServer, *storage.Storage, error) {
	stg, err := storage.NewStorage(path, storageAddress, artifactRetentionTTL, artifactRetentionRecords)
	if err != nil {
		return nil, nil, fmt.Errorf("error initializing storage: %v", err)
	}

	return startFileServer, stg, nil
}

func startFileServer(path string, address string) error {
	fs := http.FileServer(http.Dir(path))
	mux := http.NewServeMux()
	mux.Handle("/", fs)
	return http.ListenAndServe(address, mux)
}
