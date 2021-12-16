#!/bin/bash

for ((i=0; i<8; i++))
    do
    echo "PASS: out3b1-$i" > out3b1-$i
    make project3b1 >> out3b1-$i &
done