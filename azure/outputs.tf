# Consumed by deploy-aks.sh (Phase 4 of the migration plan) via
# `terraform output -raw <name>`.

output "acr_login_server" {
  description = "ACR login server, e.g. aomkubeacr.azurecr.io - used to build/tag/push images and as the image prefix in k8s/aks/*.yaml."
  value       = azurerm_container_registry.this.login_server
}

output "acr_name" {
  description = "ACR resource name - for `az acr build --registry <this>`."
  value       = azurerm_container_registry.this.name
}

output "public_ip_address" {
  description = "Static public IP for aom-lobby's LoadBalancer Service - PUBLIC_ADDR gets patched to this, and it's the address real clients Direct-Connect to."
  value       = azurerm_public_ip.lobby.ip_address
}

output "public_ip_name" {
  description = "Name of the pre-created public IP resource - referenced from k8s/aks/lobby-service.yaml so Azure attaches this specific IP rather than provisioning a new one."
  value       = azurerm_public_ip.lobby.name
}

output "aks_cluster_name" {
  description = "AKS cluster name - for `az aks get-credentials`."
  value       = azurerm_kubernetes_cluster.this.name
}

output "resource_group_name" {
  description = "Resource group name - for `az aks get-credentials` and any other resource-group-scoped az command."
  value       = azurerm_resource_group.this.name
}
