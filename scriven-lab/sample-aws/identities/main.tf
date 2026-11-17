# Lab identities. Moto enforces these policies once lab-aws.sh switches
# authentication on, so scriven's read-only check is tested for real:
#   scriven-readonly  what scriven logs in as: read and list only
#   lab-admin         stands in for the people and pipelines that change things
terraform {
  required_providers {
    aws = { source = "hashicorp/aws" }
  }
}

provider "aws" {
  s3_use_path_style = true # needed by the emulator, harmless on AWS
}

resource "aws_iam_user" "readonly" {
  name = "scriven-readonly"
}

resource "aws_iam_user_policy" "readonly" {
  name = "read-only"
  user = aws_iam_user.readonly.name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "sts:GetCallerIdentity",
        "ec2:Describe*",
        "s3:ListAllMyBuckets", "s3:ListBucket", "s3:GetBucket*", "s3:Get*Configuration", "s3:ListTagsForResource",
        "s3:HeadBucket", # Moto checks this name; on AWS, s3:ListBucket covers HeadBucket
        "iam:Get*", "iam:List*",
        "dynamodb:Describe*", "dynamodb:List*",
        "sns:Get*", "sns:List*",
        "elasticloadbalancing:Describe*",
        "rds:Describe*", "rds:ListTagsForResource",
        "lambda:Get*", "lambda:List*",
        "kms:Describe*", "kms:List*", "kms:GetKeyPolicy", "kms:GetKeyRotationStatus",
        "cloudformation:Describe*", "cloudformation:List*",
        "cloudtrail:LookupEvents",
      ]
      Resource = "*"
    }]
  })
}

resource "aws_iam_access_key" "readonly" {
  user = aws_iam_user.readonly.name
}

resource "aws_iam_user" "admin" {
  name = "lab-admin"
}

resource "aws_iam_user_policy" "admin" {
  name = "admin"
  user = aws_iam_user.admin.name
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = "*", Resource = "*" }]
  })
}

resource "aws_iam_access_key" "admin" {
  user = aws_iam_user.admin.name
}

output "readonly_access_key_id" {
  value = aws_iam_access_key.readonly.id
}

output "readonly_secret_access_key" {
  value     = aws_iam_access_key.readonly.secret
  sensitive = true
}

output "admin_access_key_id" {
  value = aws_iam_access_key.admin.id
}

output "admin_secret_access_key" {
  value     = aws_iam_access_key.admin.secret
  sensitive = true
}
