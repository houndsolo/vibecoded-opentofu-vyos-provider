terraform {
  required_providers {
    vyoscmd = {
      source  = "registry.terraform.io/houndsolo/vyoscmd"
      version = "0.1.0"
    }
  }
}

provider "vyoscmd" {
  endpoint = "https://10.20.11.11"
  # Read the API key from VYOS_API_KEY.
}
