#!/usr/bin/env bash
# Sample resources and a read-only identity for testing scriven against a real
# GCP sandbox project. Run it in Cloud Shell (it has gcloud and is signed in):
#
#   bash setup.sh <project-id> [region]
#
# Everything it creates is free while idle: a VPC network, two subnets, two
# firewall rules, an empty bucket, a Pub/Sub topic and subscription, and two
# service accounts. No VMs, NAT, load balancers or static IPs.
#
# Remove it all afterwards with: gcloud projects delete <project-id>
set -euo pipefail

PROJECT="${1:?usage: bash setup.sh <project-id> [region]}"
REGION="${2:-europe-west1}"
SA=scriven-readonly
SA_EMAIL="$SA@$PROJECT.iam.gserviceaccount.com"

gcloud config set project "$PROJECT" >/dev/null

echo "== Enabling APIs (takes a minute or two)"
gcloud services enable compute.googleapis.com storage.googleapis.com pubsub.googleapis.com \
  iam.googleapis.com cloudasset.googleapis.com cloudresourcemanager.googleapis.com \
  serviceusage.googleapis.com logging.googleapis.com

echo "== Sample resources"
gcloud compute networks create shop-vpc --subnet-mode=custom --quiet || true
gcloud compute networks subnets create shop-web --network=shop-vpc --region="$REGION" \
  --range=10.20.1.0/24 --quiet || true
gcloud compute networks subnets create shop-data --network=shop-vpc --region="$REGION" \
  --range=10.20.2.0/24 --enable-private-ip-google-access --quiet || true
gcloud compute firewall-rules create shop-allow-https --network=shop-vpc \
  --allow=tcp:443 --source-ranges=0.0.0.0/0 --target-tags=web --quiet || true
gcloud compute firewall-rules create shop-allow-internal --network=shop-vpc \
  --allow=tcp:5432 --source-ranges=10.20.1.0/24 --target-tags=db --quiet || true
gcloud storage buckets create "gs://shop-exports-$PROJECT" --location="$REGION" \
  --uniform-bucket-level-access --quiet || true
gcloud pubsub topics create shop-orders --quiet || true
gcloud pubsub subscriptions create shop-orders-worker --topic=shop-orders \
  --ack-deadline=30 --quiet || true
gcloud iam service-accounts create shop-app --display-name="Shop application" --quiet || true

echo "== Read-only identity for scriven"
gcloud iam service-accounts create "$SA" --display-name="scriven (read-only)" --quiet || true

# Metadata only: list and describe resources and read IAM policies. No role
# here can change anything, read bucket objects or pull Pub/Sub messages.
gcloud iam roles create scrivenReader --project="$PROJECT" --title="scriven reader" \
  --description="Read resource metadata and IAM policies; no data, no writes" \
  --permissions=resourcemanager.projects.get,resourcemanager.projects.getIamPolicy,\
storage.buckets.get,storage.buckets.list,storage.buckets.getIamPolicy,\
iam.serviceAccounts.get,iam.serviceAccounts.list,iam.serviceAccounts.getIamPolicy,\
iam.roles.get,iam.roles.list,serviceusage.services.get,serviceusage.services.list,\
logging.logEntries.list --quiet || true

for role in "projects/$PROJECT/roles/scrivenReader" roles/compute.viewer roles/pubsub.viewer roles/cloudasset.viewer; do
  gcloud projects add-iam-policy-binding "$PROJECT" --member="serviceAccount:$SA_EMAIL" \
    --role="$role" --condition=None --quiet >/dev/null
done
# Lets scriven ask "may I do X?" without doing X: the read-only check.
gcloud projects add-iam-policy-binding "$PROJECT" --member="serviceAccount:$SA_EMAIL" \
  --role=roles/iam.securityReviewer --condition=None --quiet >/dev/null

echo "== Key for $SA_EMAIL"
gcloud iam service-accounts keys create scriven-readonly-key.json --iam-account="$SA_EMAIL"

cat <<EOF

Done. Project: $PROJECT, region: $REGION
Next: download scriven-readonly-key.json (Cloud Shell menu > Download),
store it in Vault, then delete it here:  rm scriven-readonly-key.json
EOF
