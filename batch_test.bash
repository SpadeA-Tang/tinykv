#!/bin/bash

for ((i=0; i<=8; i++))
    do
    go test > out-$i &
done