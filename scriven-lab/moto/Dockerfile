# Moto for the scriven lab: a pinned release plus five fixes.
#
# 1. With IAM enforcement on, Moto checks S3 Control requests with the general
#    SigV4 signer, but clients sign them the S3 way (path encoded once, not
#    twice). Any S3 Control path that holds an ARN therefore fails with 403 for
#    every identity, and the Terraform AWS provider reads bucket tags through
#    exactly such a path. The patch makes S3 Control use Moto's S3 signer.
# 2. When it checks a signature, Moto falls back to a request's parsed
#    parameters when the body is empty. For Lambda's GET requests those
#    "parameters" are the headers, so every signed Lambda read fails with
#    SignatureDoesNotMatch. The patch signs GET, HEAD and DELETE requests
#    with the body they were sent with (empty), as AWS does.
# 3. Moto does not return a NAT gateway's availability mode. Since regional NAT
#    gateways (late 2025) the provider reads a missing mode as regional, and
#    then neither subnet nor Elastic IP. The patch reports "zonal", which is
#    what AWS returns for every NAT gateway created in a subnet.
# 4. An instance launched with a network-interface spec (which the provider
#    sends when associate_public_ip_address is set) reports no security
#    groups: Moto puts them on the interface only. AWS reports the primary
#    interface's groups as the instance's, and so does the patch.
# 5. ModifyInstanceAttribute with InstanceInitiatedShutdownBehavior (the
#    provider sends it after every launch that sets the attribute) falls
#    through to Moto's security-group handler with an empty list, removing
#    every group from the instance. The patch adds it to the attributes Moto
#    sets by name.
# Drop each patch once Moto does this itself.
ARG MOTO_TAG=5.2.5
FROM motoserver/moto:${MOTO_TAG}
COPY s3control_auth.py instance_groups.py /tmp/
RUN cat /tmp/s3control_auth.py >> /moto/moto/s3control/responses.py \
 && python -c "import moto.s3control.responses" \
 && grep -q "data=self._body or self._data," /moto/moto/iam/access_control.py \
 && sed -i 's/data=self._body or self._data,/data=self._body if self._method in ("GET", "HEAD", "DELETE") else (self._body or self._data),/' \
      /moto/moto/iam/access_control.py \
 && python -c "import moto.iam.access_control" \
 && echo 'NatGateway.availability_mode = "zonal"  # scriven-lab patch' >> /moto/moto/ec2/models/nat_gateways.py \
 && python -c "import moto.ec2.models.nat_gateways" \
 && cat /tmp/instance_groups.py >> /moto/moto/ec2/models/instances.py \
 && python -c "import moto.ec2.models.instances" \
 && grep -q '"DisableApiStop",' /moto/moto/ec2/responses/instances.py \
 && sed -i 's/"DisableApiStop",/"DisableApiStop", "InstanceInitiatedShutdownBehavior",/' /moto/moto/ec2/responses/instances.py \
 && python -c "import moto.ec2.responses.instances"
