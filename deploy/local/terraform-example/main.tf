# A handful of small providers to pull through the mirror. Add bigger ones
# (e.g. hashicorp/azurerm) to generate more cache traffic and exercise eviction.
terraform {
  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "3.2.2"
    }
    random = {
      source  = "hashicorp/random"
      version = "3.6.3"
    }
    local = {
      source  = "hashicorp/local"
      version = "2.5.2"
    }
  }
}

resource "random_pet" "example" {
  length = 2
}

resource "local_file" "example" {
  content  = "hello from ${random_pet.example.id}"
  filename = "${path.module}/out.txt"
}
