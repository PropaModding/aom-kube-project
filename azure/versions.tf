# Pinned 2026-08-24 against the current stable azurerm provider - check
# registry.terraform.io/providers/hashicorp/azurerm before bumping, since
# several fields this config depends on (api_server_access_profile,
# oms_agent, network_profile.network_policy) have moved/renamed across
# provider versions.
terraform {
  required_version = ">= 1.15"

  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.0"
    }
  }
}
