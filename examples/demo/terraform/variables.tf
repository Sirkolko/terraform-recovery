variable "region" {
  type    = string
  default = "eu-west-1"
}

variable "project" {
  type    = string
  default = "shop"
}

variable "env" {
  type    = string
  default = "prod"
}

variable "azs" {
  type    = list(string)
  default = ["eu-west-1a", "eu-west-1b"]
}

variable "instance_type" {
  type    = string
  default = "t3.small"
}

locals {
  name = "${var.project}-${var.env}"
}
