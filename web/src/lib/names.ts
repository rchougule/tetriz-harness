import { useMemo } from "react";
import { usePersons, useTeams } from "../api/queries";
import type { NameLookup } from "./audience";

// Falls back to the raw id so a row never renders blank while the roster loads or after a deletion.
export function useNames(): NameLookup {
  const persons = usePersons();
  const teams = useTeams();
  return useMemo(() => {
    const p = new Map((persons.data?.persons ?? []).map((x) => [x.personId, x.displayName]));
    const t = new Map((teams.data?.teams ?? []).map((x) => [x.teamId, x.name]));
    return { person: (id) => p.get(id) ?? id, team: (id) => t.get(id) ?? id };
  }, [persons.data, teams.data]);
}
