#!/bin/bash

for ((i=0; i<8; i++))
    do
    make project3b1 > out3b1-$i &
done