variable "region" {
  description = "Azure region for every resource in this config."
  type        = string
  default     = "australiaeast"
}

variable "resource_group_name" {
  description = "Dedicated resource group for this project - a clean teardown boundary for a test environment."
  type        = string
  default     = "aom-kube-test-rg"
}

variable "cluster_name" {
  description = "AKS cluster name."
  type        = string
  default     = "aom-kube-aks"
}

variable "acr_name" {
  description = "ACR name - must be globally unique across all of Azure, alphanumeric only (no hyphens)."
  type        = string
  default     = "aomkubeacr"
}

variable "node_count" {
  description = "Fixed node count for the default node pool - no autoscaler in this pass (see the migration plan's 'Explicitly deferred' section)."
  type        = number
  default     = 2
}

variable "node_vm_size" {
  description = "VM size for the default node pool. 2x Standard_D4s_v5 (4 vCPU/16GiB each) spreads pods across nodes instead of cramming onto one VM - the direct fix for the minikube swapping/restart-storm problem that motivated this migration."
  type        = string
  default     = "Standard_D4s_v5"
}

variable "aks_sku_tier" {
  description = "AKS control-plane pricing tier. Free has no SLA and no hourly control-plane cost - fine for a personal test cluster."
  type        = string
  default     = "Free"
}

variable "network_policy" {
  description = "AKS network policy engine - required for k8s NetworkPolicy resources (k8s/aks/aom-headless-netpol.yaml) to actually be enforced rather than silently no-op, same class of gotcha as minikube needing --cni=calico."
  type        = string
  default     = "azure"
}

variable "api_server_authorized_ip_ranges" {
  description = <<-EOT
    CIDRs allowed to reach the AKS API server (kubectl/management plane).
    Set to your own current public IP - find it with `curl ifconfig.me`
    or `curl api.ipify.org`. This is a dynamic residential/mobile IP for
    most people and can change; if kubectl access suddenly stops
    working, re-check your IP and update this variable, then
    `terraform apply` again (see azure/README.md's "my IP changed"
    runbook).
  EOT
  type        = list(string)
  # No default - must be set explicitly in terraform.tfvars. An empty
  # list here would mean "open to the entire internet," which should
  # never be silently defaulted to for something as sensitive as
  # cluster-admin API access.
}

variable "monthly_budget_aud" {
  description = "Monthly spend threshold (AUD) for the cost budget alert. A test cluster with 2x Standard_D4s_v5 running 24/7 costs real money - this is a notification, not a hard cap (Azure budgets can't stop spend on their own), so pair it with the az aks stop/start habit documented in azure/README.md."
  type        = number
  default     = 100
}

variable "budget_alert_email" {
  description = "Email address to notify when the budget threshold is crossed. No default - must be set explicitly in terraform.tfvars (not committed, per .gitignore)."
  type        = string
}

variable "tags" {
  description = "Common tags applied to every resource."
  type        = map(string)
  default = {
    project     = "aom-kube-project"
    environment = "test"
    managed_by  = "terraform"
  }
}
