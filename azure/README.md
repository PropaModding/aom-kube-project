# Azure infrastructure (Terraform)

Provisions the AKS cluster, ACR, static public IP, Log Analytics
workspace, and cost budget for testing this project in Azure instead of
local minikube - see the migration plan this was written from for the
full context and reasoning (host-health-agent restart storms from
running 3 concurrent heavy game pods on a single minikube VM). This
directory only provisions cloud infrastructure; the application itself
still deploys via `k8s/aks/*.yaml` + `deploy-aks.sh` (see repo root),
same as `deploy-minikube.sh` does for local dev - **that path stays
completely untouched by this migration**, so local minikube iteration
keeps working exactly as before.

## Why Terraform, not ARM

Chosen explicitly for future portability - if this project ever needs
to move to another cloud, only the `azurerm` provider resource blocks
change, not the whole templating format. The AKS/Kubernetes layer is
already cloud-agnostic by construction (standard k8s manifests).

## Prerequisites

1. `az login` - interactive, needs your own browser/account. The
   `azurerm` provider authenticates via the Azure CLI's own logged-in
   context by default (`provider.tf`'s `features {}` block), no service
   principal or client secret involved.
2. Confirm you're pointed at the right subscription:
   `az account show` (use `az account set --subscription <id>` if not).
3. Copy `terraform.tfvars.example` to `terraform.tfvars` and fill in
   real values - **at minimum**, `api_server_authorized_ip_ranges` (your
   own public IP, see below) and `budget_alert_email`.
   `terraform.tfvars` is gitignored; never commit it.

## Deploying

```
cd azure/
terraform init
terraform plan     # review before spending anything
terraform apply
```

`terraform plan` costs nothing and shows exactly what will be created -
always run it before `apply` if you've changed anything.

## What each resource is for

- **`azurerm_resource_group`** - a dedicated RG, not an existing one -
  gives a clean teardown boundary for a test environment
  (`terraform destroy` or `az group delete` cleanly removes everything).
- **`azurerm_log_analytics_workspace` + AKS's `oms_agent` block** -
  Container Insights. Directly useful for confirming the
  OOM-kill/probe-restart-storm problem that motivated this migration is
  actually gone once real load is on the cluster.
- **`azurerm_container_registry`** (Basic SKU) - image registry for the
  three app images. Basic doesn't support IP-restricted access
  (`network_rule_set` needs Premium) - a known, accepted gap, not an
  oversight; pull auth is still scoped to the AKS cluster's own kubelet
  identity via the `AcrPull` role assignment below.
- **`azurerm_public_ip`** (Standard SKU, static) - the address real
  players Direct-Connect to. Standard SKU is required to pair with
  AKS's default Standard Load Balancer; a Basic-SKU IP can't attach to
  it. Static so it's known before the app ever deploys, unlike
  minikube's `minikube ip` (read fresh, and different, on every run).
- **`azurerm_kubernetes_cluster`** - the cluster itself.
  - `network_profile { network_policy = "azure" }` - required for
    `k8s/aks/aom-headless-netpol.yaml`'s NetworkPolicy to actually be
    enforced, not silently ignored (the same class of gotcha as
    minikube needing `--cni=calico`) - verify with `az aks show` after
    apply, don't just trust the config took.
  - `api_server_access_profile.authorized_ip_ranges` - restricts
    `kubectl`/management-plane access to your own IP (see "My IP
    changed" below).
  - `sku_tier = "Free"` - no control-plane SLA cost, fine for a
    personal test cluster.
- **`azurerm_role_assignment.aks_network_contributor`** - lets the
  cluster's own control-plane identity attach the pre-created static
  public IP to a LoadBalancer Service (`k8s/aks/lobby-service.yaml`).
  Without this, that Service would fail with a permissions error.
- **`azurerm_role_assignment.aks_acr_pull`** - `AcrPull` for the AKS
  kubelet identity, defined here in Terraform rather than a manual
  `az aks update --attach-acr` step someone has to remember to re-run
  if the cluster is ever recreated.
- **`azurerm_consumption_budget_resource_group`** - a monthly spend
  notification, not a hard cap (Azure budgets can't stop spend on their
  own). Two thresholds: 80% actual, 100% forecasted. Pair this with the
  cost-control habit below.

## Emergency stop

`./emergency-stop.sh` - a fast kill switch if something's wrong
(unexpected traffic, a runaway cost signal, anything that means "stop
serving right now, figure out why after"). Three tiers, cheapest/fastest
first:

1. **`./emergency-stop.sh scale`** (default) - scales `aom-lobby` and
   `aom-headless` to 0 replicas. Seconds, not minutes. Stops all traffic
   processing immediately; the cluster/nodes keep running (still costs
   node compute), so this is a pause, easily reversed
   (`kubectl scale deployment/aom-lobby --replicas=1`).
2. **`./emergency-stop.sh cluster`** - `az aks stop`, deallocates the
   node pool's VMs entirely (stops compute billing), takes a couple of
   minutes. Everything (deployments, the Service, the static IP) stays
   defined - `az aks start` brings it all back exactly as it was.
3. **`./emergency-stop.sh nuke`** - `terraform destroy`, tears down
   *everything* (cluster, ACR and every image in it, the static IP, Log
   Analytics, the budget alert). Only for "shut this down for good," not
   incident response - asks for the same typed confirmation
   `terraform destroy` always does.

## Low-activity switch (routine cost control, not just incident response)

`./emergency-stop.sh cluster` doubles as the "off" half of a day-to-day
habit, not only an incident-response tool: stop the cluster whenever
you're not actively testing, since the VMs are the dominant cost while
idle. `./wake-cluster.sh` is the "on" half - `az aks start`, reprovisions
the node pool and control plane, a few minutes either direction. Nothing
Terraform manages needs re-applying either way; the cluster comes back
exactly as it was stopped (replica counts included).

```
./emergency-stop.sh cluster   # done testing for now
...
./wake-cluster.sh              # back to it later
```

Worth knowing before you ever need this: the game-traffic LoadBalancer
(`k8s/aks/lobby-service.yaml`) is already restricted to one IP via
`loadBalancerSourceRanges` (see the "My IP changed" section below for
what that IP is and how to update it) - real "spam from the internet"
is blocked at the network layer before it ever reaches a pod. This
script is for everything else: your own testing going wrong, a
compromised/spoofed source, or just wanting a fast, reliable stop
without having to remember the right `kubectl`/`az` incantation under
pressure.

## Cost control: `az aks stop`/`az aks start`

This cluster runs 2x `Standard_D4s_v5` nodes, which cost money whether
or not anything is actually using them. For a testing-only cluster,
stop it when you're not actively using it:

```
az aks stop  --name <cluster_name> --resource-group <resource_group_name>
az aks start --name <cluster_name> --resource-group <resource_group_name>
```

(cluster/RG names from `terraform output aks_cluster_name` /
`terraform output resource_group_name`, or your `terraform.tfvars`
values directly.) Stopped, you still pay for storage but not compute -
a meaningful saving for a cluster that's idle more than it's active.

## My IP changed

Most home/mobile ISP connections have a dynamic public IP. If `kubectl`
suddenly stops working, or you can't Direct-Connect a real client, check
whether your IP changed:

```
curl ifconfig.me
```

Then update **both** places it's restricted (they're independent, not
linked):

1. `api_server_authorized_ip_ranges` in `terraform.tfvars`, then
   `terraform apply`.
2. `loadBalancerSourceRanges` in `k8s/aks/lobby-service.yaml`, then
   `kubectl apply -f k8s/aks/lobby-service.yaml` (or re-run
   `deploy-aks.sh`).

## State

Local state for this pass (`terraform.tfstate`, gitignored) - this is a
single-user test project. Revisit a remote backend (e.g. an `azurerm`
storage-account backend) if this ever becomes team-shared or
longer-lived; not needed now.

## Tearing down

```
terraform destroy
```

Removes every resource this config created - the whole resource group
and everything in it, cleanly, since nothing else was ever put in this
RG.
