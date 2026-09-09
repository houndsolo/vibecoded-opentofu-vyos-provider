package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/houndsolo/vibecoded-opentofu-vyos-provider/internal/provider"
)

var version = "0.1.0-dev"

func main() {
	debug := flag.Bool("debug", false, "Enable debugger attachment")
	flag.Parse()
	if err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/houndsolo/vyoscmd", Debug: *debug,
	}); err != nil {
		log.Fatal(err)
	}
}
