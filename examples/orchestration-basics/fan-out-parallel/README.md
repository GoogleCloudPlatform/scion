## Fan out sample

Initialize this example as a project:

```bash
scion init
```

Create a researcher template

```bash
scion templates clone default researcher
```

Replace the system instruction in the template with the researcher prompt content 

```bash
mv ./researcher-prompt.md $(scion config dir)/templates/researcher/system-prompt.md
```

Start the workstation server

```bash
scion server start
```

enable hub and link project

```bash
scion config set hub.endpoint http://localhost:8080
scion hub enable
scion hub link
```

This will prompt you to sync templates.

Then create and start an orchestrator agent:


```bash
scion start -a orchestrator
```


Then in an orchestrator agent use the following prompt

```
Use the scion cli tool to start a researcher agent for each of the topics in topics.txt

Be sure to ask to be --notified when they are done

Once you have started each researcher wait idle for you to be notified, no need to poll or check on them, just wait for their notification.

When a research completes their work, you may delete them
```

