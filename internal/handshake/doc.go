// Package handshake implements Noise XK with the hybrid ML-KEM-768 bootstrap
// (CLAUDE.md §3.4): it turns an accepted or dialed connection into a peer
// identity and the root key of epoch 0. It knows nothing about contacts or the
// store; the responder asks its caller through the Authorizer callback.
package handshake
