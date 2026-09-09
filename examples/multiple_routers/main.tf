terraform {
  required_providers {
    vyoscmd = {
      source  = "registry.terraform.io/houndsolo/vyoscmd"
      version = "0.1.0"
    }
  }
}

variable "leaves" {
  type = map(object({
    management_ip = string
    loopback      = string
  }))
}

# VYOS_API_KEY supplies the shared API key. Use provider aliases for different
# credentials or TLS policies. Endpoints can be VM outputs unknown during plan.
provider "vyoscmd" {}

resource "vyoscmd_commands" "leaf" {
  for_each = var.leaves
  name     = each.key
  endpoint = "https://${each.value.management_ip}"

  commands = [
    "set interfaces dummy dum240 address '${each.value.loopback}'",
    "set protocols bgp system-as 800",
  ]
}
