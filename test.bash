#!/bin/bash

for ((i=0; i<5; i++))
    do
    echo "PASS: out3b1-$i" > out3b1-$i
    make singletest >> out3b1-$i &
done