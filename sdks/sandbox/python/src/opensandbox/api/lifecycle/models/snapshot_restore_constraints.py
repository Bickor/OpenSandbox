#
# Copyright 2026 The OpenSandbox Authors
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
#

from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define

from ..models.snapshot_restore_constraints_placement import SnapshotRestoreConstraintsPlacement

T = TypeVar("T", bound="SnapshotRestoreConstraints")


@_attrs_define
class SnapshotRestoreConstraints:
    """
    Attributes:
        placement (SnapshotRestoreConstraintsPlacement): Restore placement requirement.
        source_node (str): Node on which this snapshot can be restored.
        durable (bool): Whether restore is independent of the source node.
    """

    placement: SnapshotRestoreConstraintsPlacement
    source_node: str
    durable: bool

    def to_dict(self) -> dict[str, Any]:
        placement = self.placement.value

        source_node = self.source_node

        durable = self.durable

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "placement": placement,
                "sourceNode": source_node,
                "durable": durable,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        placement = SnapshotRestoreConstraintsPlacement(d.pop("placement"))

        source_node = d.pop("sourceNode")

        durable = d.pop("durable")

        snapshot_restore_constraints = cls(
            placement=placement,
            source_node=source_node,
            durable=durable,
        )

        return snapshot_restore_constraints
