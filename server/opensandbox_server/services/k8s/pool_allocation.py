# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Creation-time contract shared with the UID-bound Kubernetes allocator."""

from typing import Any, Dict, Optional

from opensandbox_server.extensions.keys import ACCESS_RENEW_EXTEND_SECONDS_KEY


ALLOCATION_MODE_ANNOTATION = "sandbox.opensandbox.io/allocation-mode"
UID_BOUND_ALLOCATION_MODE = "uid-bound-v1"
PASSIVE_POOL_EXTENSIONS = frozenset({"poolRef", ACCESS_RENEW_EXTEND_SECONDS_KEY})
RESERVED_ALLOCATION_ANNOTATIONS = frozenset(
    {
        ALLOCATION_MODE_ANNOTATION,
        "sandbox.opensandbox.io/alloc-intent",
        "sandbox.opensandbox.io/alloc-identity",
        "sandbox.opensandbox.io/alloc-status",
        "sandbox.opensandbox.io/alloc-release",
        "sandbox.opensandbox.io/alloc-released",
        "sandbox.opensandbox.io/endpoints",
    }
)


def ensure_no_allocation_annotations(annotations: Optional[Dict[str, Any]]) -> None:
    """Reject caller-provided allocation state on the protected creation path."""
    if annotations is None:
        return
    if not isinstance(annotations, dict):
        raise ValueError("Protected pool annotations must be a mapping.")
    reserved = RESERVED_ALLOCATION_ANNOTATIONS.intersection(annotations)
    if reserved:
        raise ValueError(
            "Protected pool allocation annotations are operator/controller-managed: "
            + ", ".join(sorted(reserved))
        )
