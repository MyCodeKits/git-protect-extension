#!/usr/bin/env node
// Invoked by git for remotes of the form protect::<url>.
require("../lib/run")(["remote-helper"]);
