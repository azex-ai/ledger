"use client";

import { formatAmount } from "../lib/utils";
import { useId, useState, type ReactNode } from "react";
import { Button } from "./ui/button";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "./ui/tooltip";

/** Keep the standard display bands while exposing the complete wire amount. */
export function ExactAmount({
  value,
  currencyCode = "",
  children,
}: {
  value: string;
  currencyCode?: string;
  children?: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const tooltipId = useId();
  const exact = currencyCode ? `${value} ${currencyCode}` : value;

  return (
    <Tooltip open={open} onOpenChange={setOpen}>
      <TooltipTrigger
        render={
          <Button
            variant="ghost"
            className="h-auto max-w-full flex-wrap justify-start gap-0 whitespace-normal rounded-sm border-0 p-0 hover:bg-transparent"
            style={{ font: "inherit", color: "inherit" }}
          />
        }
        aria-label={`Exact amount: ${exact}`}
        aria-describedby={open ? tooltipId : undefined}
        closeOnClick={false}
        onClick={(event) => { event.stopPropagation(); setOpen(true); }}
      >
        {children ?? formatAmount(value)}
      </TooltipTrigger>
      <TooltipContent id={tooltipId} role="tooltip" className="break-all font-mono tabular-nums">
        {exact}
      </TooltipContent>
    </Tooltip>
  );
}
