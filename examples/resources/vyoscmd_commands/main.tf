terraform {
  required_providers {
    vyoscmd = {
      source  = "registry.terraform.io/houndsolo/vyoscmd"
      version = "0.1.0"
    }
  }
}

variable "vyos_api_key" {
  type      = string
  sensitive = true
}

provider "vyoscmd" {
  endpoint = "https://10.20.11.11"
  api_key  = var.vyos_api_key
}

resource "vyoscmd_commands" "leaf_11" {
  name = "leaf-11"

  commands = [
    "set interfaces dummy dum240 address '10.255.240.11/32'",
    "set protocols bgp system-as 800",
    "delete protocols ospf",
  ]
}
