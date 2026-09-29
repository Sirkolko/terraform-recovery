data "aws_ami" "al2023" {
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = ["al2023-ami-*-x86_64"]
  }
}

resource "aws_instance" "web" {
  count = 2

  ami                    = data.aws_ami.al2023.id
  instance_type          = var.instance_type
  subnet_id              = module.network.private_subnet_ids[count.index]
  vpc_security_group_ids = [aws_security_group.web.id]
  iam_instance_profile   = aws_iam_instance_profile.web.name

  tags = {
    Name = "${local.name}-web-${count.index}"
    Role = "web"
  }

  lifecycle {
    ignore_changes = [ami]
  }
}

resource "aws_ebs_volume" "data" {
  availability_zone = var.azs[0]
  size              = 100
  type              = "gp3"
  encrypted         = true

  tags = {
    Name = "${local.name}-web-data"
  }
}

resource "aws_volume_attachment" "data" {
  device_name = "/dev/sdf"
  volume_id   = aws_ebs_volume.data.id
  instance_id = aws_instance.web[0].id
}
