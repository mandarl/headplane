import { type } from "arktype";

import Dialog, { DialogPanel } from "~/components/dialog";
import Input from "~/components/input";
import Text from "~/components/text";
import Title from "~/components/title";
import { useForm } from "~/hooks/use-form";
import type { Machine } from "~/types";

const descriptionSchema = type({
  description: "string",
});

interface ServiceDescriptionProps {
  machine: Machine;
  proto: string;
  port: number;
  currentDescription: string;
  isOpen: boolean;
  setIsOpen: (isOpen: boolean) => void;
}

export default function ServiceDescription({
  machine,
  proto,
  port,
  currentDescription,
  isOpen,
  setIsOpen,
}: ServiceDescriptionProps) {
  const form = useForm({
    schema: descriptionSchema,
    defaultValues: { description: currentDescription },
  });

  return (
    <Dialog isOpen={isOpen} onOpenChange={setIsOpen}>
      <DialogPanel>
        <Title>
          Edit description for {proto.toUpperCase()}/{port}
        </Title>
        <Text className="mb-6">
          This overrides what Tailscale reported for this service on {machine.givenName}. Clear the
          field and save to go back to the auto-detected value.
        </Text>
        <input name="action_id" type="hidden" value="update_service_description" />
        <input name="node_id" type="hidden" value={machine.id} />
        <input name="proto" type="hidden" value={proto} />
        <input name="port" type="hidden" value={port} />
        <Input
          {...form.field("description")}
          label="Description"
          placeholder="e.g. Internal admin dashboard"
        />
      </DialogPanel>
    </Dialog>
  );
}
