

# --- scriven-lab patch (see scriven-lab/moto/Dockerfile) ---------------------
def _scriven_lab_s3_signer(self, resource="*"):  # type: ignore[no-untyped-def]
    from moto.iam.access_control import S3IAMRequest

    self._authenticate_and_authorize_action(S3IAMRequest, resource)


S3ControlResponse._authenticate_and_authorize_normal_action = _scriven_lab_s3_signer  # type: ignore[method-assign]
