// Package term is pimctl's terminal etiquette in one place: whether it is
// talking to a person on each of the three streams, the colour it may use when
// it is, and the spinner it shows while the network is slow.
//
// The three streams answer three different questions, which is why
// [StdinIsTTY], [StdoutIsTTY] and [StderrIsTTY] are separate: stdin decides
// whether a prompt can be shown at all, stdout decides whether a correction
// printed a second later can still reach the reader, and stderr decides whether
// progress has anywhere to go.
//
// [Palette] is the colour side of the same question, asked per destination
// rather than per process, so a command whose stdout is a pipe can still put
// colour on stderr. Colour here is always redundant: every sequence in the
// package reinforces a glyph or a word that already carries the meaning, so
// losing it loses nothing.
//
// [Spinner] is the reason the package owns motion as well as colour. Every
// command that touches the network shows one on a terminal — a blank terminal
// during a call that can take seconds is a bug — and it deliberately ignores
// NO_COLOR, which is about colour rather than motion. Honouring NO_COLOR there
// once turned every slow command into a blank screen.
//
// The package depends on nothing inside pimctl, which is what lets
// internal/picker, internal/cli and the rest share one answer about the
// terminal instead of each working it out again.
package term
