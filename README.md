# controller-manager

Contains resources for managing controllers and artifacts.

## Usage

Currently, the function `InitServer` can be found under pkg/server/server.go. Starting this server with
the correct values will initialize a file server that can be further used to serve content.

The `pkg/storage` internals expose a storage service that can handle `Artifact` based behaviour such as
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

TODO: Extract this behaviour might not be possible since it dependent on the source, but let's look into this.
