#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2022-2026 TII (SSRC) and the Ghaf contributors
# SPDX-License-Identifier: Apache-2.0
import sys
import uuid
import os


def make_uuid(hex32):
    return str(uuid.UUID(hex=hex32))


def rename(filename, uuid):
    new = filename.replace("@u", uuid)
    os.rename(filename, new)
    print(f"{filename} -> {new}")


def main():
    if len(sys.argv) != 4:
        print(f"Usage: {sys.argv[0]} hash_file file1 file2")
        sys.exit(1)

    hash_file, file1, file2 = sys.argv[1:]

    with open(hash_file, "r") as f:
        line = f.readline().strip()

    if len(line) < 64:
        raise ValueError(
            "hash_file must contain at least 64 hex characters in first line"
        )

    h1 = line[:32]
    h2 = line[32:64]

    uuid1 = make_uuid(h1)
    uuid2 = make_uuid(h2)
    rename(file1, uuid1)
    rename(file2, uuid2)


if __name__ == "__main__":
    main()
