# tail - Go package for reading from and following continuously updated files

[![CircleCI](https://circleci.com/gh/sgtsquiggs/tail.svg?style=svg)](https://circleci.com/gh/sgtsquiggs/tail)

`tail` is forked from [`mtail`](https://github.com/google/mtail), which is a tool 
for extracting metrics from logs. This fork exists to provide the tailing function
of mtail as a go package without any of the Google-specific dependencies.

There are [other](https://godoc.org/github.com/tideland/golib/scroller) [implementations](https://godoc.org/github.com/hpcloud/tail) that achieve the same goal as this library.

### TODO:
* Documentation
* Example code