variable "subnet_ids" {
  type = list(string)
}

variable "name" {
  type = string
}

variable "sg_ids" {
  type = list(string)
}

resource "aws_instance" "web" {
  for_each               = toset(["a", "b"])
  ami                    = "ami-123"
  instance_type          = "t3.small"
  subnet_id              = var.subnet_ids[each.key == "a" ? 0 : 1]
  vpc_security_group_ids = var.sg_ids
  tags = {
    Name = "${var.name}-web-${each.key}"
  }
}

output "first_subnet" {
  value = var.subnet_ids[0]
}
