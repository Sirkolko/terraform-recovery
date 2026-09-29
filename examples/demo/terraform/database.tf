resource "aws_db_subnet_group" "main" {
  name       = "${local.name}-db"
  subnet_ids = module.network.private_subnet_ids
}

resource "aws_db_instance" "main" {
  identifier                  = "${local.name}-db"
  engine                      = "postgres"
  engine_version              = "16"
  instance_class              = "db.t4g.medium"
  allocated_storage           = 50
  storage_type                = "gp3"
  username                    = "shop"
  manage_master_user_password = true
  db_subnet_group_name        = aws_db_subnet_group.main.name
  vpc_security_group_ids      = [aws_security_group.db.id]
  multi_az                    = true
  skip_final_snapshot         = false
  final_snapshot_identifier   = "${local.name}-db-final"
}
