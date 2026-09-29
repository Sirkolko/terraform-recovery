module "network" {
  source = "./modules/network"

  name = local.name
  cidr = "10.40.0.0/16"
  azs  = var.azs
}
