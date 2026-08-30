resource "azurerm_resource_group" "this" {
  name     = var.resource_group_name
  location = var.region
  tags     = var.tags
}

# Container Insights target for the AKS cluster's oms_agent block below -
# directly useful for confirming the OOM-kill/probe-restart-storm problem
# that motivated this migration is actually gone (Phase 5 of the
# migration plan watches this after scaling to 3 aom-headless replicas).
resource "azurerm_log_analytics_workspace" "this" {
  name                = "${var.cluster_name}-logs"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  sku                 = "PerGB2018"
  retention_in_days   = 30
  tags                = var.tags
}

# Basic SKU - cheapest, comfortably fits the current ~2.85GB image set
# (aom-k8s ~2.82GB, aom-lobby/host-health-agent ~16MB each). Known
# limitation, not fixed here: Basic doesn't support network_rule_set
# (IP-restricted access) at all - that needs Premium. ACR stays
# reachable by anyone with valid credentials; pull auth is scoped to the
# AKS kubelet identity via the role assignment below, not left open.
resource "azurerm_container_registry" "this" {
  name                = var.acr_name
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  sku                 = "Basic"
  admin_enabled       = false
  tags                = var.tags
}

# Standard SKU required to pair with AKS's default Standard Load
# Balancer - a Basic-SKU public IP can't attach to it. Static allocation
# so the address is known before the app ever deploys (feeds
# PUBLIC_ADDR in k8s/aks/lobby-deployment.yaml, patched by
# deploy-aks.sh) rather than the minikube idiom of reading `minikube ip`
# fresh every run.
resource "azurerm_public_ip" "lobby" {
  name                = "${var.cluster_name}-lobby-pip"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  allocation_method   = "Static"
  sku                 = "Standard"
  tags                = var.tags
}

resource "azurerm_kubernetes_cluster" "this" {
  name                = var.cluster_name
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  dns_prefix          = var.cluster_name
  sku_tier            = var.aks_sku_tier
  tags                = var.tags

  default_node_pool {
    name       = "default"
    node_count = var.node_count
    vm_size    = var.node_vm_size
  }

  # System-assigned: this is the cluster/control-plane identity (distinct
  # from kubelet_identity below), used by AKS to manage Azure resources
  # on the cluster's behalf - e.g. attaching azurerm_public_ip.lobby to a
  # LoadBalancer Service, which is why it needs Network Contributor on
  # this resource group (see the role assignment below).
  identity {
    type = "SystemAssigned"
  }

  # Azure CNI + Azure network policy - required for k8s NetworkPolicy
  # resources (k8s/aks/aom-headless-netpol.yaml) to actually be enforced.
  # Same class of gotcha as minikube silently no-op'ing NetworkPolicy
  # without --cni=calico - verify this actually took effect with
  # `az aks show` per the migration plan's Phase 1 verification step,
  # don't just trust this config took.
  network_profile {
    network_plugin = "azure"
    network_policy = var.network_policy
  }

  # Empty list would mean "open to the entire internet" for cluster-admin
  # API access - var.api_server_authorized_ip_ranges has no default for
  # exactly this reason, forcing an explicit value in terraform.tfvars.
  api_server_access_profile {
    authorized_ip_ranges = var.api_server_authorized_ip_ranges
  }

  oms_agent {
    log_analytics_workspace_id = azurerm_log_analytics_workspace.this.id
  }
}

# Lets the AKS cluster's own control-plane identity attach the
# pre-created static public IP (azurerm_public_ip.lobby) to a
# LoadBalancer Service - without this, k8s/aks/lobby-service.yaml's
# reference to that IP would fail with a permissions error. Scoped to
# the whole resource group (simplest - the PIP lives here) rather than
# just the PIP resource, since this RG holds nothing else sensitive.
resource "azurerm_role_assignment" "aks_network_contributor" {
  scope                = azurerm_resource_group.this.id
  role_definition_name = "Network Contributor"
  principal_id         = azurerm_kubernetes_cluster.this.identity[0].principal_id
}

# ACR pull auth, defined here rather than a manual `az aks update
# --attach-acr` step - per explicit preference for IaC as the real
# source of truth, so this isn't a step someone has to remember to
# re-run if the cluster is ever recreated. kubelet_identity is the
# node-level identity actually used for image pulls (distinct from the
# cluster identity above) - AKS auto-creates it since the
# kubelet_identity block itself isn't set in the cluster resource, but
# it's still exposed as a read attribute here.
resource "azurerm_role_assignment" "aks_acr_pull" {
  scope                = azurerm_container_registry.this.id
  role_definition_name = "AcrPull"
  principal_id         = azurerm_kubernetes_cluster.this.kubelet_identity[0].object_id
}

# Cost control is a notification, not a hard cap - Azure budgets can't
# stop spend on their own. Pair this with the `az aks stop`/`az aks
# start` habit documented in azure/README.md for an idle test cluster.
# start_date must be the first of a month; using the current month at
# apply time (not a fixed hardcoded date) so this works correctly
# whenever this config is first applied - lifecycle.ignore_changes
# stops that from causing a perpetual diff/replacement on every
# subsequent `terraform plan` once it's set.
resource "azurerm_consumption_budget_resource_group" "this" {
  name              = "${var.resource_group_name}-budget"
  resource_group_id = azurerm_resource_group.this.id
  amount            = var.monthly_budget_aud
  time_grain        = "Monthly"

  time_period {
    start_date = formatdate("YYYY-MM-01'T'00:00:00Z", timestamp())
  }

  notification {
    enabled        = true
    threshold      = 80
    operator       = "GreaterThan"
    threshold_type = "Actual"
    contact_emails = [var.budget_alert_email]
  }

  notification {
    enabled        = true
    threshold      = 100
    operator       = "GreaterThan"
    threshold_type = "Forecasted"
    contact_emails = [var.budget_alert_email]
  }

  lifecycle {
    ignore_changes = [time_period[0].start_date]
  }
}
