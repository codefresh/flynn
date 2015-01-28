package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	_ "github.com/flynn/flynn/Godeps/_workspace/src/github.com/docker/docker/daemon/graphdriver/aufs"
	_ "github.com/flynn/flynn/Godeps/_workspace/src/github.com/docker/docker/daemon/graphdriver/btrfs"
	_ "github.com/flynn/flynn/Godeps/_workspace/src/github.com/docker/docker/daemon/graphdriver/devmapper"
	_ "github.com/flynn/flynn/Godeps/_workspace/src/github.com/docker/docker/daemon/graphdriver/vfs"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/flynn/go-docopt"
	tuf "github.com/flynn/flynn/Godeps/_workspace/src/github.com/flynn/go-tuf/client"
	"github.com/flynn/flynn/pinkerton"
)

func init() {
	Register("download", runDownload, `
usage: flynn-host download [--driver=<name>] [--root=<path>] [--repository=<uri>] [--tuf-db=<path>]

Options:
  -d --driver=<name>     image storage driver [default: aufs]
  -r --root=<path>       image storage root [default: /var/lib/docker]
  -u --repository=<uri>  image repository URI [default: https://dl.flynn.io/images]
  -t --tuf-db=<path>     local TUF file [default: /etc/flynn/tuf.db]

Download container images from a TUF repository`)
}

type tufFile struct {
	bytes.Buffer
}

func (t *tufFile) Delete() error {
	return nil
}

func runDownload(args *docopt.Args) error {
	if err := os.MkdirAll(args.String["--root"], 0755); err != nil {
		return fmt.Errorf("error creating root dir: %s", err)
	}

	local, err := tuf.FileLocalStore(args.String["--tuf-db"])
	if err != nil {
		return err
	}
	remote, err := tuf.HTTPRemoteStore(args.String["--repository"], nil)
	if err != nil {
		return err
	}
	client := tuf.NewClient(local, remote)
	if _, err := client.Update(); err != nil && !tuf.IsLatestSnapshot(err) {
		return err
	}
	var f tufFile
	if err := client.Download("/version.json", &f); err != nil {
		return err
	}

	var manifest map[string]string
	if err := json.Unmarshal(f.Bytes(), &manifest); err != nil {
		return err
	}

	ctx, err := pinkerton.BuildContext(args.String["--driver"], args.String["--root"])
	if err != nil {
		return err
	}

	for name, id := range manifest {
		fmt.Printf("Downloading %s %s...\n", name, id)
		url := fmt.Sprintf("%s?name=%s&id=%s", args.String["--repository"], name, id)
		if err := ctx.PullTUF(url, client, pinkerton.InfoPrinter(false)); err != nil {
			return err
		}
	}
	return nil
}
