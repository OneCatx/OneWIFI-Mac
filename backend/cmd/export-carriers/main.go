// Offline deterministic export of the pinned carrier registry for Rust.
package main

import (
	"flag"
	"log"
	"os"

	"proletarius.local/core/internal/vowifi/carrier"
)

func main() {
	output := flag.String("output", "", "generated JSON output path")
	cases := flag.Bool("cases", false, "export synthetic Go selection cases instead of registry")
	flag.Parse()
	if *output == "" {
		log.Fatal("-output is required")
	}
	file, err := os.Create(*output)
	if err != nil {
		log.Fatal(err)
	}
	export := carrier.ExportRegistry
	if *cases {
		export = carrier.ExportSelectionCases
	}
	if err := export(file); err != nil {
		_ = file.Close()
		log.Fatal(err)
	}
	if err := file.Close(); err != nil {
		log.Fatal(err)
	}
}
