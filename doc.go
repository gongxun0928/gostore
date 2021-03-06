/*
Package storage implement a collection of storage algorithm and
necessary tools and libraries.

api:

Interface specification to access gostore datastructures.

bubt:

A version of Bayer-Tree for sorting and retrieving {key,value} entries.
Index built bottoms up, hence Bottoms Up BTree, and kept immutable for
rest of its lifetime.

bogn:

Multi leveled, LSM based, ACID compliant, storage package.

flock:

File locking library for linux, mac and windows. Similar to sync.RWMutex
and works across processes.

lib:

Convinience functions that can be used by other packages. Package shall
not import packages other than golang's standard packages.

llrb:

A version of Left Leaning Red Black tree for sorting and retrieving
{key,value} entries. Index resides entirely in memory with optional
Multi Version Concurrency Control.

lsm:

Implement Log-structured-merge between two indexes.

malloc:

Custom memory management for storage algorithms.
*/
package storage
