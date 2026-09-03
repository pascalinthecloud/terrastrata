---
title: Point your clients at it
description: The network_mirror block Terraform and OpenTofu need, and the https requirement behind it.
---

Provider caching needs no changes to your Terraform code — only a CLI
configuration file on each agent.

## The CLI config

Add this to `~/.terraformrc` (Terraform) or `~/.tofurc` (OpenTofu), or inject it
in your pipeline and point `TF_CLI_CONFIG_FILE` at it:

```hcl
provider_installation {
  network_mirror {
    url     = "https://tf-mirror.internal/" # your Ingress/Gateway hostname
    include = ["registry.terraform.io/*/*"]
  }
  direct {
    exclude = ["registry.terraform.io/*/*"]
  }
}
```

The `include`/`exclude` pair is what makes the mirror authoritative for those
providers: `include` sends matching addresses to the mirror, and the matching
`exclude` on `direct` stops the CLI from silently falling back to the registry —
which would hide the fact that your mirror is not being used.

:::caution[The mirror URL must be https]
Terraform and OpenTofu refuse a plaintext `network_mirror` URL. terrastrata
serves plain HTTP by design, so point clients at the TLS-terminating Ingress or
Gateway in front of it, with a certificate the agents trust.
:::

## Mirroring both ecosystems

`include` patterns are matched per source address, so list every registry you
mirror:

```hcl
provider_installation {
  network_mirror {
    url     = "https://tf-mirror.internal/"
    include = ["registry.terraform.io/*/*", "registry.opentofu.org/*/*"]
  }
  direct {
    exclude = ["registry.terraform.io/*/*", "registry.opentofu.org/*/*"]
  }
}
```

See [mirroring several registries](/terrastrata/guides/multiple-registries/) for
the server side.

## Confirm it worked

```bash
terraform init
# Initializing provider plugins...
# - Installing hashicorp/azurerm v3.110.0 from https://tf-mirror.internal/...
```

The `from https://tf-mirror.internal/...` line is the proof. If it says
`from registry.terraform.io`, the `include` pattern did not match the source
address you are using — check [troubleshooting](/terrastrata/guides/troubleshooting/).

## Authentication

Terraform's `network_mirror` client sends no credentials at all, so `AUTH_TOKEN`
is only useful behind a gateway that injects the header, or for non-Terraform
consumers. Treat network policy as the boundary for provider traffic. Module
registry endpoints are different — Terraform does send `credentials` there, and
that path is covered in the [module registry guide](/terrastrata/guides/module-registry/).
