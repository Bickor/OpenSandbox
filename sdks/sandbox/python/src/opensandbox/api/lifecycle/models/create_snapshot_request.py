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

from ..models.create_snapshot_request_format import CreateSnapshotRequestFormat
from ..types import UNSET, Unset

T = TypeVar("T", bound="CreateSnapshotRequest")


@_attrs_define
class CreateSnapshotRequest:
    """Optional settings for creating a sandbox snapshot.

    Attributes:
        name (str | Unset): Optional human-readable snapshot name.
        format_ (CreateSnapshotRequestFormat | Unset): Requested snapshot representation. If omitted, the current
            runtime
            and controller auto-selection behavior is preserved.
    """

    name: str | Unset = UNSET
    format_: CreateSnapshotRequestFormat | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        name = self.name

        format_: str | Unset = UNSET
        if not isinstance(self.format_, Unset):
            format_ = self.format_.value

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if name is not UNSET:
            field_dict["name"] = name
        if format_ is not UNSET:
            field_dict["format"] = format_

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        name = d.pop("name", UNSET)

        _format_ = d.pop("format", UNSET)
        format_: CreateSnapshotRequestFormat | Unset
        if isinstance(_format_, Unset):
            format_ = UNSET
        else:
            format_ = CreateSnapshotRequestFormat(_format_)

        create_snapshot_request = cls(
            name=name,
            format_=format_,
        )

        return create_snapshot_request
