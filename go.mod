module github.com/sujaykumarsuman/airlift

go 1.26

// npm packages occasionally ship Go files; keep ./... out of them.
ignore ./web/node_modules

require rsc.io/qr v0.2.0
