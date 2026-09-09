# multi fixture

A small tree with a binary, an empty file, an executable, a path with spaces
and some UTF-8, packed by `tools/repobundle.py` in both formats. The Go bundle
stage must restore it byte for byte and mode for mode.
