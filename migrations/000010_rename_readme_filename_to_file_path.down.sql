-- Copyright 2020 The Go Authors. All rights reserved.
-- Use of this source code is governed by a BSD-style
-- license that can be found in the LICENSE file.

BEGIN;

ALTER TABLE readmes RENAME COLUMN file_path TO filename;

END;