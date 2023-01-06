package WebsocketReverseProxy

type OnDataFlow func(uid string, length int64)
