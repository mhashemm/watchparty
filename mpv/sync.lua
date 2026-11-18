-- The node's own way into mpv (mpv/ORDER.md): mpv runs it in order with everything else it
-- does, and what it says reaches the node as client-message events, which mpv never merges.
-- It decides nothing; the rules are in Go.

-- hello: the node learns this script's name by asking everyone, as a user's script with the
-- same file name would take the name and leave this one another
mp.register_script_message("watchparty-hello", function()
    mp.commandv("script-message", "watchparty", "hello", mp.get_script_name())
end)
