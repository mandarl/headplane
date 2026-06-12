import { useEffect } from "react";
import { useNavigate } from "react-router";

import Button from "~/components/button";
import Card from "~/components/card";
import Code from "~/components/code";
import Input from "~/components/input";
import Link from "~/components/link";

interface UserPromptProps {
  hostname: string;
}

export default function UserPrompt({ hostname }: UserPromptProps) {
  const navigate = useNavigate();

  // Start downloading and instantiating the WASM while the user types their
  // username. The result is stored in a module-level variable in wasm.client.ts
  // so loadHeadplaneWASM() returns immediately when SSHConsole mounts.
  useEffect(() => {
    import("./wasm.client").then(({ loadHeadplaneWASM }) => {
      loadHeadplaneWASM().catch(() => {
        // Ignore errors here — SSHConsole will surface them properly.
      });
    });
  }, []);

  function handleSubmit(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const formData = new FormData(e.currentTarget);
    const username = formData.get("user");
    if (!username) return;

    // Client-side navigation keeps the JS module cache alive so the WASM
    // that started loading above is still instantiated when SSHConsole mounts.
    // The shouldRevalidate function in page.tsx ensures the loader runs once
    // to create a fresh pre-auth key for this transition.
    navigate(`?user=${encodeURIComponent(username.toString())}`);
  }

  return (
    <div className="flex h-screen items-center justify-center">
      <Card>
        <Card.Title>Enter Username</Card.Title>
        <Card.Text className="mb-4">
          Enter the username you want to use to connect to <Code>{hostname}</Code>
          {". "}
          SSH via the web follows the same ACL rules as regular SSH access in Headscale, so only
          permitted usernames will work.
          <br />
          <br />
          See the{" "}
          <Link external styled to="https://headplane.net/features/ssh#troubleshooting">
            troubleshooting guide
          </Link>{" "}
          for common errors.
        </Card.Text>
        <form onSubmit={handleSubmit}>
          <Input
            labelHidden
            type="text"
            label="Username"
            name="user"
            placeholder="Username"
            className="mb-2"
            required
          />
          <Button type="submit" variant="heavy" className="w-full">
            Connect
          </Button>
        </form>
      </Card>
    </div>
  );
}
