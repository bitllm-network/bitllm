package node

// Version is the program version, set at release time with
// -ldflags "-X github.com/bitllm-network/bitllm/internal/node.Version=vX.Y.Z".
// It goes into the announce, so old and new nodes can be told apart.
var Version = "dev"
