# Moto for the scriven lab: a pinned release plus one fix.
#
# With IAM enforcement on, Moto checks S3 Control requests with the general
# SigV4 signer, but clients sign them the S3 way (path encoded once, not
# twice). Any S3 Control path that holds an ARN therefore fails with 403 for
# every identity, and the Terraform AWS provider reads bucket tags through
# exactly such a path. The patch makes S3 Control use Moto's S3 signer.
# Drop it once Moto does this itself.
ARG MOTO_TAG=5.2.5
FROM motoserver/moto:${MOTO_TAG}
COPY s3control_auth.py /tmp/s3control_auth.py
RUN cat /tmp/s3control_auth.py >> /moto/moto/s3control/responses.py \
 && python -c "import moto.s3control.responses"
