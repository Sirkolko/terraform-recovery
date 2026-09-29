resource "aws_iam_role" "web" {
  name = "${local.name}-web"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "ec2.amazonaws.com" }
    }]
  })
}

resource "aws_iam_instance_profile" "web" {
  name = "${local.name}-web"
  role = aws_iam_role.web.name
}

resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.web.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_policy" "assets_read" {
  name        = "${local.name}-assets-read"
  description = "Read access to the assets bucket"

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["s3:GetObject"]
      Resource = "${aws_s3_bucket.assets.arn}/*"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "assets_read" {
  role       = aws_iam_role.web.name
  policy_arn = aws_iam_policy.assets_read.arn
}
