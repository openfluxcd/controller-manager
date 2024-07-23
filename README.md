# controller-manager

Contains resources for managing controllers and artifacts.

## Usage

Currently, the function `InitServer` can be found under server/server.go. Starting this server with
the correct values will initialize a file server that can be further used to serve content.

The `storage` internals expose a storage service that can handle `Artifact` based behaviour such as
- storing
- garbage collection
- storage locks
- artifact verification
- check for existing artifacts

This can be used by an Artifact handler to create storage the following way:

```go
// Garbage collect previous advertised artifact(s) from storage
_ = r.garbageCollect(ctx, obj)
// Create artifact
artifact := r.Storage.NewArtifactFor(obj.Kind, obj, metadata.Revision,
    fmt.Sprintf("%s.tar.gz", r.digestFromRevision(metadata.Revision)))	
	
...

// ensure artifact exists
if err := r.Storage.MkdirAll(artifact); err != nil {}

...

// lock the store
unlock, err := r.Storage.Lock(artifact)
if err != nil {...}
defer unlock()

// Either CopyFromPath or Archive the Artifact.
done
```

To make this somewhat more consumable a two method interface has been created called `Storer`.

`Storer` has the following two methods:

```go
type Storer interface {
	// ReconcileStorage is responsible for setting up Storage data like URLs.
	ReconcileStorage(ctx context.Context, obj Collectable, artifact *v1.Artifact) error
	ReconcileArtifact(ctx context.Context, obj Collectable, revision, dir, hash string, archiveFunc func(v1.Artifact) error) error
}
```

`ReconcileStorage` should be called first to garbage collect old artifacts and set up artifact URLs.
`ReconcileArtifact` should be called next to create artifacts and store actual data. The storage function is there to
create individual storage preferences for certain artifacts. Such as, specific ways of storing OCI layers or Archives.
