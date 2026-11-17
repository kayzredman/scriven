# Sample AWS resources for scriven to capture. Applied to the lab's Moto
# emulator by ../../lab-aws.sh; the endpoint and credentials come from the
# environment, so nothing here is specific to the emulator.
terraform {
  required_providers {
    aws = { source = "hashicorp/aws" }
  }
}

provider "aws" {
  s3_use_path_style = true # needed by the emulator, harmless on AWS
}

# ../../lab-aws.sh --drift sets this to change the account behind scriven's
# back: one resource changed, one added, one removed.
variable "drift" {
  type    = bool
  default = false
}

resource "aws_vpc" "shop" {
  cidr_block           = "10.20.0.0/16"
  enable_dns_hostnames = true
  tags                 = { Name = "shop", team = "payments" }
}

resource "aws_subnet" "shop_a" {
  vpc_id            = aws_vpc.shop.id
  cidr_block        = "10.20.1.0/24"
  availability_zone = "eu-west-1a"
  tags              = { Name = "shop-a" }
}

resource "aws_subnet" "shop_b" {
  vpc_id                  = aws_vpc.shop.id
  cidr_block              = "10.20.2.0/24"
  availability_zone       = "eu-west-1b"
  map_public_ip_on_launch = true
  tags                    = { Name = "shop-b" }
}

resource "aws_internet_gateway" "shop" {
  vpc_id = aws_vpc.shop.id
  tags   = { Name = "shop" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.shop.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.shop.id
  }
  tags = { Name = "shop-public" }
}

resource "aws_route_table_association" "b" {
  subnet_id      = aws_subnet.shop_b.id
  route_table_id = aws_route_table.public.id
}

resource "aws_security_group" "web" {
  name        = "shop-web"
  description = "Web tier"
  vpc_id      = aws_vpc.shop.id
  ingress {
    description = "https"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  dynamic "ingress" {
    for_each = var.drift ? [1] : []
    content {
      description = "ssh, opened by hand"
      from_port   = 22
      to_port     = 22
      protocol    = "tcp"
      cidr_blocks = ["0.0.0.0/0"]
    }
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
  tags = { Name = "shop-web" }
}

resource "aws_security_group" "db" {
  name        = "shop-db"
  description = "Database tier: reachable from the web tier only"
  vpc_id      = aws_vpc.shop.id
  ingress {
    description     = "postgres from web"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.web.id]
  }
  tags = { Name = "shop-db" }
}

resource "aws_s3_bucket" "assets" {
  bucket = "shop-assets-lab"
}

resource "aws_s3_bucket_versioning" "assets" {
  bucket = aws_s3_bucket.assets.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_iam_role" "app" {
  name = "shop-app"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = "sts:AssumeRole", Principal = { Service = "ec2.amazonaws.com" } }]
  })
  tags = { team = "payments" }
}

resource "aws_iam_policy" "assets_read" {
  name = "shop-assets-read"
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["s3:GetObject", "s3:ListBucket"]
      Resource = [aws_s3_bucket.assets.arn, "${aws_s3_bucket.assets.arn}/*"]
    }]
  })
}

resource "aws_iam_role_policy_attachment" "app" {
  role       = aws_iam_role.app.name
  policy_arn = aws_iam_policy.assets_read.arn
}

resource "aws_iam_role_policy" "orders" {
  name = "orders-table"
  role = aws_iam_role.app.name
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = ["dynamodb:GetItem", "dynamodb:PutItem"], Resource = aws_dynamodb_table.orders.arn }]
  })
}

resource "aws_dynamodb_table" "orders" {
  name         = "shop-orders"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "order_id"
  attribute {
    name = "order_id"
    type = "S"
  }
  tags = { team = "payments" }
}

resource "aws_sns_topic" "alerts" {
  count = var.drift ? 0 : 1
  name  = "shop-alerts"
}

resource "aws_s3_bucket" "exports" {
  count  = var.drift ? 1 : 0
  bucket = "shop-exports-lab"
}
