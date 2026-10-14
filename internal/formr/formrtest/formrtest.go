// Package formrtest holds fixtures shared by formr's tests: a synthetic provider schema and a
// helper that writes a module tree into a temporary directory.
//
// The provider is invented (`acme/acme`) on purpose. The pilot module is the first real
// acceptance case, and a fixture shaped like it would let a Hetzner-specific assumption pass
// every test.
package formrtest

import (
	"os"
	"path/filepath"
	"testing"
)

// Provider is the synthetic provider's address.
const Provider = "registry.terraform.io/acme/acme"

// Schema is a `providers schema -json` document for the synthetic provider. It covers every
// shape the inventory has to handle: required, optional, optional-and-computed, computed-only,
// sensitive and write-only attributes; a set block and a single block; an object-typed
// attribute; and a nested-type attribute on a data source.
const Schema = `{
  "format_version": "1.0",
  "provider_schemas": {
    "registry.terraform.io/acme/acme": {
      "provider": {
        "version": 0,
        "block": {
          "attributes": {
            "token": {"type": "string", "optional": true, "sensitive": true, "description": "API token."},
            "endpoint": {"type": "string", "optional": true}
          }
        }
      },
      "resource_schemas": {
        "acme_server": {
          "version": 0,
          "block": {
            "attributes": {
              "id": {"type": "string", "optional": true, "computed": true},
              "name": {"type": "string", "required": true, "description": "Server name."},
              "size": {"type": "string", "optional": true},
              "labels": {"type": ["map", "string"], "optional": true},
              "ip": {"type": "string", "computed": true},
              "secret": {"type": "string", "optional": true, "sensitive": true, "write_only": true},
              "settings": {"type": ["object", {"mode": "string", "level": "number"}], "optional": true}
            },
            "block_types": {
              "network": {
                "nesting_mode": "set",
                "block": {
                  "attributes": {
                    "ip": {"type": "string", "optional": true, "computed": true},
                    "mac": {"type": "string", "computed": true},
                    "network_id": {"type": "number", "optional": true}
                  }
                }
              },
              "timeouts": {
                "nesting_mode": "single",
                "block": {"attributes": {"create": {"type": "string", "optional": true}}}
              }
            }
          }
        },
        "acme_volume": {
          "version": 0,
          "block": {
            "attributes": {
              "id": {"type": "string", "computed": true},
              "name": {"type": "string", "required": true},
              "size": {"type": "number", "optional": true}
            }
          }
        },
        "acme_unused": {
          "version": 0,
          "block": {"attributes": {"name": {"type": "string", "required": true}}}
        }
      },
      "data_source_schemas": {
        "acme_image": {
          "version": 0,
          "block": {
            "attributes": {
              "name": {"type": "string", "optional": true},
              "id": {"type": "string", "computed": true},
              "arch": {"type": "string", "computed": true},
              "zones": {
                "nested_type": {
                  "nesting_mode": "list",
                  "attributes": {"name": {"type": "string", "computed": true}}
                },
                "computed": true
              }
            }
          }
        }
      }
    }
  }
}`

// Main is a root module exercising the tracer. Literal values that must never reach a report
// contain the word SYNTHETIC.
var Main = map[string]string{
	"versions.tf": `
terraform {
  required_providers {
    acme = { source = "acme/acme" }
  }
}
`,
	"variables.tf": `
variable "create" {
  type    = bool
  default = true
}
variable "name" { type = string }
variable "size" {
  type    = string
  default = null
}
variable "labels" {
  type    = map(string)
  default = {}
}
variable "networks" {
  type    = list(object({ ip = string, network_id = number }))
  default = []
}
variable "secret" {
  type      = string
  sensitive = true
  default   = "SYNTHETIC-SENSITIVE-DEFAULT"
}
`,
	"main.tf": `
locals {
  tags = merge(var.labels, { managed_by = "SYNTHETIC-FORCED-VALUE" })
}

data "acme_image" "os" {
  name = "SYNTHETIC-IMAGE-NAME"
}

resource "acme_server" "this" {
  count  = var.create ? 1 : 0
  name   = var.name
  size   = var.size
  labels = local.tags
  secret = var.secret

  dynamic "network" {
    for_each = var.networks
    iterator = net
    content {
      ip         = net.value.ip
      network_id = net.value.network_id
    }
  }

  settings = {
    mode  = "SYNTHETIC-MODE"
    level = length(var.name)
  }
}

resource "acme_volume" "a" {
  name = var.name
  size = 10
}

resource "acme_volume" "b" {
  name = var.name
}

module "first" {
  source = "./child"
  label  = var.name
}

module "second" {
  source = "./child"
}
`,
	"outputs.tf": `
output "ips" {
  value = try(acme_server.this[0].network[*].ip, [])
}
output "server_id" {
  value = try(acme_server.this[0].id, null)
}
output "image" {
  value = data.acme_image.os.id
}
`,
	"child/main.tf": `
terraform {
  required_providers {
    acme = { source = "acme/acme" }
  }
}

variable "label" {
  type    = string
  default = "SYNTHETIC-CHILD-DEFAULT"
}

resource "acme_volume" "c" {
  name = var.label
}

output "id" {
  value = acme_volume.c.id
}
`,
}

// WriteModule writes files (slash-separated relative paths to contents) under a temporary
// directory and returns it.
func WriteModule(t testing.TB, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// With returns a copy of files with overrides applied; an empty value deletes a file.
func With(files map[string]string, overrides map[string]string) map[string]string {
	out := make(map[string]string, len(files)+len(overrides))
	for k, v := range files {
		out[k] = v
	}
	for k, v := range overrides {
		if v == "" {
			delete(out, k)
			continue
		}
		out[k] = v
	}

	return out
}
