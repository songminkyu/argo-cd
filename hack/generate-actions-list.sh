#!/usr/bin/env bash
find resource_customizations -name action.lua \
    | sed 's~resource_customizations/\(.*\)/actions/\(.*\)/action.lua~- [\1/\2](https://github.com/argoproj/argo-cd/blob/master/resource_customizations/\1/actions/\2/action.lua)~' \
    | sort \
    | uniq \
    > docs/operator-manual/resource_actions_builtin.md
