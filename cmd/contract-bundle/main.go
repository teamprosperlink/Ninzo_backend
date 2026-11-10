// Command contract-bundle writes the single-file contracts in contracts/dist, and
// the Bruno collection in contracts/bruno.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/teamprosperlink/Ninzo_backend/internal/contractbundle"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	bundles, err := contractbundle.All("contracts")
	if err != nil {
		return err
	}
	for name, data := range bundles {
		if err := write(filepath.Join("contracts", "dist", name), data); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(filepath.Join("contracts", "bruno")); err != nil {
		return err
	}
	for app, file := range map[string]string{"customer": "customer.openapi.yaml", "rider": "rider.openapi.yaml", "partner": "partner.openapi.yaml", "console": "console.openapi.yaml"} {
		files, err := contractbundle.Bruno(app, bundles[file])
		if err != nil {
			return err
		}
		for rel, content := range files {
			if err := write(filepath.Join("contracts", "bruno", filepath.FromSlash(rel)), []byte(content)); err != nil {
				return err
			}
		}
	}
	fmt.Println("wrote contracts/dist and contracts/bruno")
	return nil
}

func write(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
