# Authenticates via the Azure CLI's own logged-in context (`az login`) -
# no service principal/client secret configured here deliberately, since
# this is a single-user test project and a CLI-auth'd human identity is
# simpler and has nothing to leak into state or version control.
provider "azurerm" {
  features {}
}
