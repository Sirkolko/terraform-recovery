terraform {
  required_version = ">= 1.5"
  backend "s3" {
    bucket = "state"
    key    = "prod.tfstate"
    region = "eu-west-1"
  }
}

provider "aws" {
  region = var.region
  default_tags {
    tags = {
      Project   = var.project
      ManagedBy = "terraform"
    }
  }
}

provider "aws" {
  alias  = "use1"
  region = "us-east-1"
}

variable "region" {
  default = "eu-west-1"
}

variable "project" {
  type = string
}

variable "azs" {
  type    = list(string)
  default = ["eu-west-1a", "eu-west-1b"]
}

variable "db_password" {
  type      = string
  sensitive = true
  default   = "hunter2"
}

locals {
  name     = "${var.project}-${terraform.workspace}"
  vpc_cidr = "10.20.0.0/16"
  common   = { Owner = "platform" }
}

resource "aws_vpc" "main" {
  cidr_block = local.vpc_cidr
  tags       = merge(local.common, { Name = "${local.name}-vpc" })
}

resource "aws_subnet" "public" {
  count             = length(var.azs)
  vpc_id            = aws_vpc.main.id
  cidr_block        = cidrsubnet(local.vpc_cidr, 8, count.index)
  availability_zone = var.azs[count.index]
  tags = {
    Name = "${local.name}-public-${count.index}"
  }
}

resource "aws_route_table_association" "public" {
  for_each       = { for i, s in aws_subnet.public : tostring(i) => s }
  subnet_id      = each.value.id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
}

resource "aws_s3_bucket" "logs" {
  provider = aws.use1
  bucket   = "${local.name}-logs"
}

resource "aws_s3_bucket_versioning" "logs" {
  provider = aws.use1
  bucket   = aws_s3_bucket.logs.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_db_instance" "main" {
  identifier             = "${local.name}-db"
  password               = var.db_password
  vpc_security_group_ids = [aws_security_group.db.id]
  tags                   = merge(var.extra_tags, { Name = "db" })
}

variable "extra_tags" {
  type = map(string)
}

resource "aws_security_group" "db" {
  name   = "${local.name}-db"
  vpc_id = aws_vpc.main.id
  dynamic "ingress" {
    for_each = [5432]
    content {
      from_port       = ingress.value
      to_port         = ingress.value
      protocol        = "tcp"
      security_groups = [aws_security_group.app.id]
    }
  }
}

resource "aws_security_group" "app" {
  name_prefix = "app-"
  vpc_id      = aws_vpc.main.id
}

module "compute" {
  source     = "./modules/compute"
  subnet_ids = aws_subnet.public[*].id
  name       = local.name
  sg_ids     = [aws_security_group.app.id]
}

data "aws_ami" "al2" {
  most_recent = true
}

resource "aws_instance" "bastion" {
  ami           = data.aws_ami.al2.id
  instance_type = "t3.micro"
  subnet_id     = module.compute.first_subnet
  tags = {
    Name = "bastion"
  }
}

resource "aws_instance" "dynamic_count" {
  count         = length(data.aws_ami.al2.block_device_mappings)
  instance_type = "t3.nano"
}

resource "random_password" "db" {
  length = 16
}

import {
  to = aws_vpc.main
  id = "vpc-existing"
}
