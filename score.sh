#!/bin/bash

go run ./generate_std_usage.go && go tool nm output/std_usage/main | grep ' T ' | wc -l
