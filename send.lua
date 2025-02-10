wrk.method = "POST"
wrk.body   = '{"SenderID":"user1","ReceiverID":"user2","Text":"Hello"}'
wrk.headers["Content-Type"] = "application/json"