

# --- scriven-lab patch (see scriven-lab/moto/Dockerfile) ---------------------
_scriven_lab_attach_eni = Instance.attach_eni


def _scriven_lab_attach_eni_groups(self, eni, device_index):  # type: ignore[no-untyped-def]
    attachment = _scriven_lab_attach_eni(self, eni, device_index)
    if int(device_index) == 0 and not self.security_groups:
        self.security_groups = list(eni.group_set)
    return attachment


Instance.attach_eni = _scriven_lab_attach_eni_groups  # type: ignore[method-assign]
