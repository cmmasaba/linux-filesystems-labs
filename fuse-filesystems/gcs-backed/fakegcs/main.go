// Command fakegcs serves an in-memory GCS emulator (fake-gcs-server) on
// localhost, for trying gcs.out without a GCP project:
//
//	go run ./gcs-backed/fakegcs -bucket lab &
//	STORAGE_EMULATOR_HOST=127.0.0.1:4443 ./gcs.out --bucket lab /tmp/gcsfs
//
// Known gap: the emulator ignores generation preconditions on copy and
// delete, so rename/unlink conflict handling can only be checked on real GCS.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/fsouza/fake-gcs-server/fakestorage"
)

func main() {
	bucket := flag.String("bucket", "lab", "bucket to create")
	port := flag.Uint("port", 4443, "port to listen on (127.0.0.1 only)")
	flag.Parse()

	addr := fmt.Sprintf("127.0.0.1:%d", *port)

	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		Scheme:      "http",
		Host:        "127.0.0.1",
		Port:        uint16(*port),
		ExternalURL: "http://" + addr,
		// The Go client downloads objects with XML-style GET /<bucket>/<object>
		// requests, which the emulator only routes when the Host header
		// matches PublicHost (default storage.googleapis.com).
		PublicHost: addr,
	})
	if err != nil {
		log.Fatal(err)
	}

	srv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: *bucket})
	fmt.Printf("fake GCS at %s with bucket %q; export STORAGE_EMULATOR_HOST=127.0.0.1:%d\n", srv.URL(), *bucket, *port)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	srv.Stop()
}
