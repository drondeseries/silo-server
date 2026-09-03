import { useEffect, useMemo, useRef, useState } from "react";
import type { Library, PageSectionConfig } from "@/api/types";
import {
  useAdminSections,
  useBulkCreateSections,
  useCreateSection,
  useUpdateSection,
  useDeleteSection,
  useDeleteSections,
  useReorderSections,
  useRestoreDefaultSections,
} from "@/hooks/queries/sections";
import RecipeGalleryModal from "@/components/RecipeGallery/RecipeGalleryModal";
import RecipeConfigDrawer from "@/components/RecipeGallery/RecipeConfigDrawer";
import type { AddPayload } from "@/components/RecipeGallery/RecipeConfigDrawer";
import type { RecipeDefinition, GalleryPreset } from "@/lib/recipes";
import { fetchRecipeCatalog } from "@/lib/recipes";
import { useQuery } from "@tanstack/react-query";
import { useAdminCollections, useImportTraktCollection } from "@/hooks/queries/admin/collections";
import { useAdminLibraries } from "@/hooks/queries/admin/libraries";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Loader2, Plus, RotateCcw, Trash2 } from "lucide-react";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { toast } from "sonner";
import { buildSectionReorderEntries } from "./adminSectionOrder";
import SectionEditorDrawer from "@/components/sections/SectionEditorDrawer";
import {
  SectionDragOverlay,
  SortableSectionTableRow,
  type EditableSectionViewModel,
} from "@/components/sections/EditableSectionRows";
import {
  DndContext,
  DragOverlay,
  PointerSensor,
  KeyboardSensor,
  closestCenter,
  useSensor,
  useSensors,
} from "@dnd-kit/core";
import type { DragStartEvent, DragEndEvent } from "@dnd-kit/core";
import { SortableContext, verticalListSortingStrategy, arrayMove } from "@dnd-kit/sortable";
import { sortableKeyboardCoordinates } from "@dnd-kit/sortable";
import type { LibraryCollection } from "@/api/types";
import { updateCheckboxSelection } from "@/lib/checkboxSelection";

function formatCollectionOptionLabel(
  collection: { title: string; library_id: number },
  libraries: Library[],
): string {
  const library = libraries.find((entry) => entry.id === collection.library_id);
  return library ? `${collection.title} (${library.name})` : collection.title;
}

function LibraryPicker({
  libraries,
  value,
  onChange,
}: {
  libraries: Library[];
  value: number | null;
  onChange: (libraryId: number) => void;
}) {
  return (
    <Select
      value={value ? String(value) : undefined}
      onValueChange={(next) => onChange(Number(next))}
    >
      <SelectTrigger className="w-[220px]">
        <SelectValue placeholder="Choose library" />
      </SelectTrigger>
      <SelectContent>
        {libraries.map((library) => (
          <SelectItem key={library.id} value={String(library.id)}>
            {library.name}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

function toEditableSection(section: PageSectionConfig): EditableSectionViewModel {
  return {
    id: section.id,
    title: section.title,
    sectionType: section.section_type,
    itemLimit: section.item_limit,
    featured: section.featured,
    enabled: section.enabled,
    isCustom: true,
    config: section.config,
  };
}

interface TraktPublicRecipeConfig {
  preset: "trending" | "popular";
  mediaType: "movie" | "tv";
}

function getTraktRecipeConfig(config: Record<string, unknown>): TraktPublicRecipeConfig | null {
  if (config.source_provider !== "trakt") return null;
  const preset = config.source_preset;
  const mediaType = config.media_type;
  if (
    (preset !== "trending" && preset !== "popular") ||
    (mediaType !== "movie" && mediaType !== "tv")
  ) {
    return null;
  }
  return { preset, mediaType };
}

function isMatchingTraktCollection(
  collection: LibraryCollection,
  preset: string,
  mediaType: string,
  libraryID: number,
) {
  return (
    collection.management_mode === "section" &&
    collection.management_key ===
      buildTraktSectionManagedCollectionKey(preset, mediaType, libraryID) &&
    collection.collection_type === "trakt" &&
    collection.library_id === libraryID &&
    collection.source_config?.preset === preset &&
    collection.source_config?.media_type === mediaType
  );
}

function buildTraktSectionManagedCollectionKey(
  preset: string,
  mediaType: string,
  libraryID: number,
) {
  return `trakt:${preset}:${mediaType}:library:${libraryID}`;
}

function findDefaultTraktLibrary(libraries: Library[], mediaType: string): number | null {
  const wantedType = mediaType === "tv" ? "series" : "movies";
  return libraries.find((library) => library.type === wantedType)?.id ?? libraries[0]?.id ?? null;
}

export default function AdminSections() {
  const [scope, setScope] = useState("home");
  const { data: librariesData } = useAdminLibraries();
  const librariesList = useMemo(() => librariesData ?? [], [librariesData]);
  const [selectedLibraryId, setSelectedLibraryId] = useState<number | null>(null);
  const activeLibraryId = scope === "library" ? selectedLibraryId : null;
  const { data, isLoading } = useAdminSections(scope, activeLibraryId ?? undefined);
  const { data: collectionsData = [] } = useAdminCollections();
  const { data: recipeCatalog } = useQuery({
    queryKey: ["recipe-catalog"],
    queryFn: fetchRecipeCatalog,
    staleTime: 5 * 60 * 1000,
  });
  const collectionLabels = new Map(
    collectionsData.map((collection) => [
      collection.id,
      formatCollectionOptionLabel(collection, librariesList),
    ]),
  );
  const [dialogOpen, setDialogOpen] = useState(false);
  const [editingSection, setEditingSection] = useState<PageSectionConfig | null>(null);
  const [orderedSections, setOrderedSections] = useState<PageSectionConfig[]>([]);
  const [activeId, setActiveId] = useState<string | null>(null);
  const [confirmDeleteSection, setConfirmDeleteSection] = useState<PageSectionConfig | null>(null);
  const [confirmDeleteSelected, setConfirmDeleteSelected] = useState(false);
  const [confirmDeleteAll, setConfirmDeleteAll] = useState(false);
  const [selectedSectionIds, setSelectedSectionIds] = useState<Set<string>>(new Set());
  const selectionAnchorRef = useRef<string | null>(null);
  const deleteMutation = useDeleteSection();
  const deleteSectionsMutation = useDeleteSections();
  const reorderMutation = useReorderSections();
  const restoreDefaultsMutation = useRestoreDefaultSections();
  const [confirmRestoreOpen, setConfirmRestoreOpen] = useState(false);
  const [resetProfiles, setResetProfiles] = useState(false);
  const [galleryOpen, setGalleryOpen] = useState(false);
  const [pickedRecipe, setPickedRecipe] = useState<{
    def: RecipeDefinition;
    preset: GalleryPreset;
  } | null>(null);
  const createFromGalleryMutation = useCreateSection();
  const importTraktMutation = useImportTraktCollection();
  const createMutation = useCreateSection();
  const bulkCreateMutation = useBulkCreateSections();
  const updateMutation = useUpdateSection();

  const sections = useMemo(() => data?.sections ?? [], [data?.sections]);
  const orderedSectionIds = useMemo(
    () => orderedSections.map((section) => section.id),
    [orderedSections],
  );
  const selectedSections = useMemo(
    () => orderedSections.filter((section) => selectedSectionIds.has(section.id)),
    [orderedSections, selectedSectionIds],
  );
  const isHomeScope = scope === "home";
  const canManageCurrentScope =
    isHomeScope || (librariesList.length > 0 && selectedLibraryId !== null);
  const canDrag = !reorderMutation.isPending && canManageCurrentScope;

  useEffect(() => {
    if (librariesList.length === 0) {
      setSelectedLibraryId(null);
      return;
    }

    setSelectedLibraryId((current) => {
      if (current && librariesList.some((library) => library.id === current)) {
        return current;
      }
      return librariesList[0]?.id ?? null;
    });
  }, [librariesList]);

  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 5 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );

  useEffect(() => {
    setOrderedSections(sections);
  }, [sections]);

  useEffect(() => {
    const clearSelection = (event: KeyboardEvent) => {
      if (
        event.key === "Escape" &&
        confirmDeleteSection === null &&
        !confirmDeleteSelected &&
        !confirmDeleteAll
      ) {
        setSelectedSectionIds(new Set());
        selectionAnchorRef.current = null;
      }
    };
    window.addEventListener("keydown", clearSelection);
    return () => window.removeEventListener("keydown", clearSelection);
  }, [confirmDeleteAll, confirmDeleteSection, confirmDeleteSelected]);

  function clearSectionSelection() {
    setSelectedSectionIds(new Set());
    selectionAnchorRef.current = null;
  }

  function handleScopeChange(nextScope: string) {
    clearSectionSelection();
    setScope(nextScope);
  }

  function handleLibraryChange(libraryId: number) {
    clearSectionSelection();
    setSelectedLibraryId(libraryId);
  }

  function updateSectionSelection(sectionId: string, checked: boolean, extendRange: boolean) {
    const anchorId = extendRange && selectedSectionIds.size > 0 ? selectionAnchorRef.current : null;
    setSelectedSectionIds((previous) =>
      updateCheckboxSelection(
        previous,
        orderedSectionIds,
        anchorId,
        sectionId,
        checked,
        extendRange,
      ),
    );
    if (anchorId === null || !orderedSectionIds.includes(anchorId)) {
      selectionAnchorRef.current = sectionId;
    }
  }

  function handleDelete(section: PageSectionConfig) {
    setConfirmDeleteSection(section);
  }

  function handleEdit(section: PageSectionConfig) {
    setEditingSection(section);
    setDialogOpen(true);
  }

  function handleDragStart(event: DragStartEvent) {
    setActiveId(event.active.id as string);
  }

  function handleDragEnd(event: DragEndEvent) {
    setActiveId(null);

    const { active, over } = event;
    if (!over || active.id === over.id) return;

    const oldIndex = orderedSections.findIndex((s) => s.id === active.id);
    const newIndex = orderedSections.findIndex((s) => s.id === over.id);
    if (oldIndex === -1 || newIndex === -1) return;

    const nextSections = arrayMove(orderedSections, oldIndex, newIndex);
    setOrderedSections(nextSections);
    reorderMutation.mutate(buildSectionReorderEntries(nextSections), {
      onError: () => {
        setOrderedSections(sections);
      },
    });
  }

  function handleDragCancel() {
    setActiveId(null);
  }

  const activeSection = activeId ? (orderedSections.find((s) => s.id === activeId) ?? null) : null;
  const selectedLibraryName = librariesList.find((library) => library.id === activeLibraryId)?.name;
  const sectionScopeLabel = isHomeScope ? "home" : `${selectedLibraryName ?? "library"} library`;
  const sectionDeletionNotice =
    "Silo will also try to remove section-managed collections that are no longer referenced. This action cannot be undone.";
  const deleteAllDescription = isHomeScope
    ? `Delete all ${orderedSections.length} home sections? ${sectionDeletionNotice}`
    : `Delete all ${orderedSections.length} sections shown for ${selectedLibraryName ?? "this library"}? ${sectionDeletionNotice}`;
  const deleteSelectedDescription = `Delete ${selectedSections.length} selected section${selectedSections.length === 1 ? "" : "s"}? ${sectionDeletionNotice}`;
  const deleteProgressLabel = `Deleting ${deleteSectionsMutation.progress?.completed ?? 0} of ${deleteSectionsMutation.progress?.total ?? orderedSections.length} sections`;

  function handleDeleteSelectedSections() {
    if (canManageCurrentScope && selectedSections.length > 0) {
      deleteSectionsMutation.mutate(selectedSections.map((section) => section.id));
    }
    setConfirmDeleteSelected(false);
  }

  function handleDeleteAllSections() {
    if (canManageCurrentScope && orderedSections.length > 0) {
      deleteSectionsMutation.mutate(orderedSections.map((section) => section.id));
    }
    setConfirmDeleteAll(false);
  }

  function normalizeLibraryIDs(ids: number[] | undefined): number[] {
    if (!ids || ids.length === 0) return [];
    return Array.from(new Set(ids.filter((id) => Number.isInteger(id) && id > 0)));
  }

  async function ensureTraktSectionCollection(
    payload: AddPayload,
    traktRecipe: TraktPublicRecipeConfig,
    libraryID: number,
  ): Promise<LibraryCollection> {
    const existing = collectionsData.find((collection) =>
      isMatchingTraktCollection(collection, traktRecipe.preset, traktRecipe.mediaType, libraryID),
    );
    if (existing) return existing;

    const managementKey = buildTraktSectionManagedCollectionKey(
      traktRecipe.preset,
      traktRecipe.mediaType,
      libraryID,
    );
    const imported = await importTraktMutation.mutateAsync({
      body: {
        library_id: libraryID,
        title: payload.title,
        description: "",
        preset: traktRecipe.preset,
        media_type: traktRecipe.mediaType,
        limit: payload.item_limit,
        featured: payload.featured,
        management_mode: "section",
        management_source: "recipe_gallery",
        management_key: managementKey,
      },
    });
    return imported.collection;
  }

  async function createBulkSectionsFromGallery(
    payload: AddPayload,
    libraryIDs: number[],
  ): Promise<void> {
    if (libraryIDs.length === 0) {
      throw new Error("Choose at least one library before applying this section");
    }

    const config = payload.config;
    const traktRecipe = getTraktRecipeConfig(config);
    const selectedCollectionID =
      typeof config.library_collection_id === "string" ? config.library_collection_id.trim() : "";
    if (traktRecipe && selectedCollectionID === "") {
      for (const libraryID of libraryIDs) {
        const collection = await ensureTraktSectionCollection(payload, traktRecipe, libraryID);
        await createMutation.mutateAsync({
          scope: "library",
          library_id: libraryID,
          section_type: payload.section_type,
          title: payload.title,
          item_limit: payload.item_limit,
          featured: payload.featured,
          enabled: payload.enabled,
          config: { ...config, library_collection_id: collection.id },
        });
      }
      toast.success(`Created ${libraryIDs.length} section${libraryIDs.length === 1 ? "" : "s"}`);
      return;
    }

    const result = await bulkCreateMutation.mutateAsync({
      scope: "library",
      library_ids: libraryIDs,
      section_type: payload.section_type,
      title: payload.title,
      item_limit: payload.item_limit,
      featured: payload.featured,
      enabled: payload.enabled,
      config,
    });
    toast.success(`Created ${result.created} section${result.created === 1 ? "" : "s"}`);
  }

  async function createSectionFromGallery(payload: AddPayload) {
    const bulkLibraryIDs = normalizeLibraryIDs(payload.library_ids);
    if (payload.apply_to_all_libraries || bulkLibraryIDs.length > 0) {
      await createBulkSectionsFromGallery(payload, bulkLibraryIDs);
      return;
    }

    let config = payload.config;
    const traktRecipe = getTraktRecipeConfig(config);
    const selectedCollectionID =
      typeof config.library_collection_id === "string" ? config.library_collection_id.trim() : "";
    if (traktRecipe && selectedCollectionID === "") {
      const targetLibraryID =
        activeLibraryId ?? findDefaultTraktLibrary(librariesList, traktRecipe.mediaType);
      if (!targetLibraryID) {
        throw new Error("Choose a library before adding this Trakt section");
      }
      const collection = await ensureTraktSectionCollection(payload, traktRecipe, targetLibraryID);
      config = { ...config, library_collection_id: collection.id };
    }

    const data: Partial<PageSectionConfig> = {
      scope,
      ...(scope === "library" && activeLibraryId != null ? { library_id: activeLibraryId } : {}),
      section_type: payload.section_type,
      title: payload.title,
      item_limit: payload.item_limit,
      featured: payload.featured,
      enabled: payload.enabled,
      config,
    };
    await createFromGalleryMutation.mutateAsync(data);
  }

  if (isLoading) return <div className="p-4">Loading sections...</div>;

  return (
    <div
      className="space-y-6"
      aria-busy={deleteSectionsMutation.isPending}
      inert={deleteSectionsMutation.isPending ? true : undefined}
    >
      <ConfirmDialog
        open={confirmDeleteSection !== null}
        onOpenChange={(open) => {
          if (!open) setConfirmDeleteSection(null);
        }}
        title="Delete section"
        description={`Delete section "${confirmDeleteSection?.title}"? Silo will also try to remove any section-managed collection that is no longer referenced. This action cannot be undone.`}
        confirmLabel="Delete"
        variant="destructive"
        onConfirm={() => {
          if (confirmDeleteSection) deleteMutation.mutate(confirmDeleteSection.id);
          setConfirmDeleteSection(null);
        }}
      />
      <ConfirmDialog
        open={confirmDeleteSelected}
        onOpenChange={setConfirmDeleteSelected}
        title="Delete selected sections"
        description={deleteSelectedDescription}
        confirmLabel="Delete selected"
        variant="destructive"
        onConfirm={handleDeleteSelectedSections}
      />
      <ConfirmDialog
        open={confirmDeleteAll}
        onOpenChange={setConfirmDeleteAll}
        title="Delete all sections"
        description={deleteAllDescription}
        confirmLabel="Delete all"
        variant="destructive"
        onConfirm={handleDeleteAllSections}
      />
      <Dialog
        open={confirmRestoreOpen}
        onOpenChange={(open) => {
          setConfirmRestoreOpen(open);
          if (!open) setResetProfiles(false);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Restore Default Sections</DialogTitle>
          </DialogHeader>
          <div className="space-y-4">
            <p className="text-muted-foreground text-sm">
              This will replace all {scope === "home" ? "home" : "library"} sections with the
              defaults. Any custom sections will be removed.
            </p>
            <div className="flex items-center gap-2">
              <Switch
                id="resetProfiles"
                size="sm"
                checked={resetProfiles}
                onCheckedChange={(checked) => setResetProfiles(checked === true)}
              />
              <Label htmlFor="resetProfiles" className="text-sm font-normal">
                Also reset all user customizations for this scope
              </Label>
            </div>
            <div className="flex justify-end gap-2">
              <Button
                variant="outline"
                onClick={() => {
                  setConfirmRestoreOpen(false);
                  setResetProfiles(false);
                }}
              >
                Cancel
              </Button>
              <Button
                variant="destructive"
                disabled={restoreDefaultsMutation.isPending}
                onClick={() => {
                  restoreDefaultsMutation.mutate(
                    {
                      scope,
                      ...(activeLibraryId != null ? { library_id: activeLibraryId } : {}),
                      reset_profiles: resetProfiles,
                    },
                    {
                      onSuccess: () => {
                        toast.success("Sections restored to defaults");
                        setConfirmRestoreOpen(false);
                        setResetProfiles(false);
                      },
                      onError: () => {
                        toast.error("Failed to restore defaults");
                      },
                    },
                  );
                }}
              >
                Restore Defaults
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
      <div className="page-header gap-5">
        <div className="space-y-3">
          <h1 className="page-title text-[clamp(2rem,4vw,3rem)]">Sections</h1>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button
            size="sm"
            variant="outline"
            disabled={!canManageCurrentScope || restoreDefaultsMutation.isPending}
            onClick={() => setConfirmRestoreOpen(true)}
          >
            <RotateCcw className="mr-1 h-4 w-4" /> Restore Defaults
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={!canManageCurrentScope}
            onClick={() => setGalleryOpen(true)}
          >
            <Plus className="mr-1 h-4 w-4" /> Add from Gallery
          </Button>
          <Button
            size="sm"
            disabled={!canManageCurrentScope}
            onClick={() => {
              setEditingSection(null);
              setDialogOpen(true);
            }}
          >
            <Plus className="mr-1 h-4 w-4" /> Add Section
          </Button>
          {selectedSections.length > 0 ? (
            <>
              <Badge variant="secondary">{selectedSections.length} selected</Badge>
              <Button size="sm" variant="ghost" onClick={clearSectionSelection}>
                Clear
              </Button>
              <Button
                size="sm"
                variant="destructive"
                disabled={deleteSectionsMutation.isPending || !canManageCurrentScope}
                onClick={() => setConfirmDeleteSelected(true)}
              >
                <Trash2 data-icon="inline-start" /> Delete Selected
              </Button>
            </>
          ) : null}
          {orderedSections.length > 0 ? (
            <Button
              size="sm"
              variant="destructive"
              disabled={
                deleteSectionsMutation.isPending ||
                restoreDefaultsMutation.isPending ||
                !canManageCurrentScope
              }
              onClick={() => setConfirmDeleteAll(true)}
            >
              {deleteSectionsMutation.isPending ? (
                <Loader2 className="mr-1 h-4 w-4 animate-spin" />
              ) : (
                <Trash2 className="mr-1 h-4 w-4" />
              )}
              {deleteSectionsMutation.isPending ? `${deleteProgressLabel}…` : "Delete All"}
            </Button>
          ) : null}
        </div>
      </div>

      <Tabs value={scope} onValueChange={handleScopeChange}>
        <TabsList>
          <TabsTrigger value="home">Home</TabsTrigger>
          <TabsTrigger value="library">Library</TabsTrigger>
        </TabsList>
      </Tabs>

      {!isHomeScope && (
        <div className="flex items-center gap-3">
          <Label>Library</Label>
          {librariesList.length > 0 ? (
            <LibraryPicker
              libraries={librariesList}
              value={selectedLibraryId}
              onChange={handleLibraryChange}
            />
          ) : (
            <p className="text-muted-foreground text-sm">
              Create a library before configuring Library sections.
            </p>
          )}
        </div>
      )}

      {canDrag && (
        <p className="text-muted-foreground text-sm">
          Drag and drop sections to change their order.
        </p>
      )}

      <DndContext
        sensors={canDrag ? sensors : []}
        collisionDetection={closestCenter}
        onDragStart={handleDragStart}
        onDragEnd={handleDragEnd}
        onDragCancel={handleDragCancel}
      >
        <div className="surface-panel overflow-x-auto rounded-2xl border-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-10">
                  <span className="sr-only">Select</span>
                </TableHead>
                <TableHead className="w-8"></TableHead>
                <TableHead>Title</TableHead>
                <TableHead>Type</TableHead>
                <TableHead className="w-20">Items</TableHead>
                <TableHead className="w-20">Featured</TableHead>
                <TableHead className="w-20">Enabled</TableHead>
                <TableHead className="w-24">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <SortableContext
              items={orderedSections.map((s) => s.id)}
              strategy={verticalListSortingStrategy}
            >
              <TableBody>
                {orderedSections.map((section) => (
                  <SortableSectionTableRow
                    key={section.id}
                    section={toEditableSection(section)}
                    canReorder={canDrag}
                    libraries={librariesList}
                    collectionLabels={collectionLabels}
                    catalog={recipeCatalog}
                    selected={selectedSectionIds.has(section.id)}
                    selectionLabel={`Select ${section.title} ${sectionScopeLabel} section`}
                    onSelectionChange={(checked, extendRange) =>
                      updateSectionSelection(section.id, checked, extendRange)
                    }
                    onEdit={() => handleEdit(section)}
                    onDelete={() => handleDelete(section)}
                  />
                ))}
                {orderedSections.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={8} className="text-muted-foreground py-8 text-center">
                      No sections configured for {scope} scope.
                    </TableCell>
                  </TableRow>
                )}
              </TableBody>
            </SortableContext>
          </Table>
        </div>
        <DragOverlay>
          {activeSection ? (
            <SectionDragOverlay
              section={toEditableSection(activeSection)}
              catalog={recipeCatalog}
            />
          ) : null}
        </DragOverlay>
      </DndContext>

      <SectionEditorDrawer
        mode="admin"
        open={dialogOpen}
        onOpenChange={(open) => {
          setDialogOpen(open);
          if (!open) setEditingSection(null);
        }}
        section={editingSection}
        scope={scope}
        currentLibraryId={activeLibraryId}
        libraries={librariesList}
        recipeCatalog={recipeCatalog}
        isSubmitting={
          createMutation.isPending || updateMutation.isPending || bulkCreateMutation.isPending
        }
        onSave={(section) => {
          if (section.id) {
            updateMutation.mutate(
              { id: section.id, ...section },
              {
                onSuccess: () => {
                  setDialogOpen(false);
                  setEditingSection(null);
                },
                onError: (error) => {
                  toast.error(error instanceof Error ? error.message : "Failed to update section");
                },
              },
            );
          } else {
            createMutation.mutate(section, {
              onSuccess: () => {
                setDialogOpen(false);
                setEditingSection(null);
              },
              onError: (error) => {
                toast.error(error instanceof Error ? error.message : "Failed to create section");
              },
            });
          }
        }}
      />

      <RecipeGalleryModal
        open={galleryOpen}
        onClose={() => setGalleryOpen(false)}
        onPick={(def, preset) => {
          setGalleryOpen(false);
          setPickedRecipe({ def, preset });
        }}
      />

      {pickedRecipe && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
          <RecipeConfigDrawer
            def={pickedRecipe.def}
            preset={pickedRecipe.preset}
            onCancel={() => setPickedRecipe(null)}
            onBackToGallery={() => {
              setPickedRecipe(null);
              setGalleryOpen(true);
            }}
            onAdd={async (payload) => {
              try {
                await createSectionFromGallery(payload);
                setPickedRecipe(null);
              } catch (error) {
                toast.error(error instanceof Error ? error.message : "Failed to create section");
                throw error;
              }
            }}
          />
        </div>
      )}
    </div>
  );
}
