#!/bin/sh

# Don't try to go get subpackages of ifacemaker.
go list -f '{{ join .Imports "\n"}}{{"\n"}}{{ join .TestImports "\n" }}{{"\n"}}{{ join .XTestImports "\n"}}' ./... | grep -v "github.com/sumia01/ifacemaker" | xargs go get -v
